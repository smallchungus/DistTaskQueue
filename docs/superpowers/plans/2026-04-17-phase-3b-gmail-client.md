# Phase 3B — Gmail Client

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Build the Gmail-side I/O the workers need: load OAuth tokens (from Phase 3A storage), construct an authenticated `*gmail.Service`, list new INBOX/PRIMARY message IDs since a given history cursor, and fetch a single message in raw MIME. Save refreshed tokens back to Postgres automatically. No Google requests in tests — `httptest.Server` plays Gmail.

**Architecture:**

```
internal/gmail/
  token.go              LoadToken / SaveToken / savingSource wrapper
  token_test.go         pure unit (oauth2.Token round-trip via mock TokenSource)
  client.go             Config, New(ctx, cfg), Client struct, LatestMessageIDs, FetchMessage
  client_integration_test.go    httptest.Server playing Gmail API; real Postgres for token storage
```

Token flow:
1. `LoadToken(ctx, store, userID, key)` reads `oauth_tokens` row, decrypts both halves, returns `*oauth2.Token`.
2. `oauth2.Config.TokenSource(ctx, tok)` wraps the loaded token with refresh capability — when expired, oauth2 hits Google's token endpoint, gets a new access token, and emits it via Token().
3. Our `savingSource` wraps that, calls Token() through it, and on every successful return diffs against the previously-saved token; if changed, encrypts and UPSERTs back via `SaveToken`. Idempotent — re-saving the same token is a no-op.
4. The saving source feeds `oauth2.NewClient(ctx, ts)` to get an `*http.Client` that auto-refreshes.
5. `gmail.NewService(ctx, option.WithHTTPClient(client), option.WithEndpoint(cfg.Endpoint))` produces the *gmail.Service. `Endpoint` defaults to Google; tests pass `httptest.Server.URL`.

`LatestMessageIDs` calls `Users.History.List("me").StartHistoryId(uint64(lastHistoryID)).HistoryTypes("messageAdded").LabelId("INBOX")`, walks the result, filters to messages with `CATEGORY_PERSONAL` label, returns IDs + new cursor (the largest `historyId` seen). Empty list + same cursor when nothing new.

`FetchMessage` calls `Users.Messages.Get("me", id).Format("raw")`, decodes the base64url-encoded `Raw` field, returns raw MIME bytes.

**Tech Stack:** `google.golang.org/api/gmail/v1`, `google.golang.org/api/option`, `golang.org/x/oauth2`, `golang.org/x/oauth2/google` (for the Google `Endpoint` only — no real OAuth flow yet).

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` Sections 4 and 5.

**Branch:** `phase-3b-gmail-client` → PR to `main`.

---

## Section A — Branch + dependencies

### Task A1: Branch

- [ ] **Step 1**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3b-gmail-client
```

### Task A2: Add modules

- [ ] **Step 1**

```bash
go get google.golang.org/api/gmail/v1
go get google.golang.org/api/option
go get golang.org/x/oauth2
go get golang.org/x/oauth2/google
go mod tidy
```

- [ ] **Step 2: Verify build**

```bash
go build ./...
```

Expected: silent success.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "chore(gmail): add google api gmail/v1 and oauth2 deps"
```

---

## Section B — Token helpers (TDD, pure unit)

### Task B1: Failing tests

**Files:**
- Create: `internal/gmail/token_test.go`

- [ ] **Step 1**

```go
package gmail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

// fakeStore stubs the methods savingSource uses. It records the last token saved
// so tests can assert SaveToken was called with the right value.
type fakeStore struct {
	saved   *oauth2.Token
	saveErr error
}

// inMemoryTokenSource returns a sequence of tokens on successive Token() calls.
type sequenceSource struct {
	tokens []*oauth2.Token
	idx    int
	err    error
}

func (s *sequenceSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.idx >= len(s.tokens) {
		return s.tokens[len(s.tokens)-1], nil
	}
	tok := s.tokens[s.idx]
	s.idx++
	return tok, nil
}

func TestSavingSource_SavesWhenTokenChanges(t *testing.T) {
	tok1 := &oauth2.Token{AccessToken: "v1", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour)}
	tok2 := &oauth2.Token{AccessToken: "v2", RefreshToken: "r1", Expiry: time.Now().Add(2 * time.Hour)}

	base := &sequenceSource{tokens: []*oauth2.Token{tok1, tok2}}
	var saved []*oauth2.Token
	src := newSavingSource(base, func(t *oauth2.Token) error {
		saved = append(saved, t)
		return nil
	}, tok1)

	got, err := src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "v1" {
		t.Fatalf("first token: %s", got.AccessToken)
	}
	if len(saved) != 0 {
		t.Fatalf("save called for unchanged token: %d", len(saved))
	}

	got, err = src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "v2" {
		t.Fatalf("second token: %s", got.AccessToken)
	}
	if len(saved) != 1 || saved[0].AccessToken != "v2" {
		t.Fatalf("save not called with refreshed token: %+v", saved)
	}
}

func TestSavingSource_PropagatesBaseError(t *testing.T) {
	base := &sequenceSource{err: errors.New("refresh failed")}
	src := newSavingSource(base, func(*oauth2.Token) error { return nil }, nil)
	if _, err := src.Token(); err == nil {
		t.Fatal("expected error")
	}
}

// LoadToken / SaveToken integration test against real Postgres lives in
// client_integration_test.go (it needs a store, which needs testcontainers).
// Skipped here to keep this file pure-unit.

func TestEncryptToken_DecryptToken_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	in := &oauth2.Token{
		AccessToken:  "the-access",
		RefreshToken: "the-refresh",
		Expiry:       time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}

	accessCT, refreshCT, err := encryptToken(in, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	out, err := decryptToken(accessCT, refreshCT, in.Expiry, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if out.AccessToken != in.AccessToken || out.RefreshToken != in.RefreshToken {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
}

// fakeStore not actually used in these unit tests — placeholder for the
// integration tests in client_integration_test.go.
var _ = fakeStore{}
var _ = uuid.UUID{}
var _ = context.Background()
var _ = store.OAuthToken{}
```

Run `go test ./internal/gmail/...` — expect undefined `newSavingSource`, `encryptToken`, `decryptToken`. Capture last 3 lines.

### Task B2: Implement token helpers

**Files:**
- Create: `internal/gmail/token.go`

- [ ] **Step 1**

```go
package gmail

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

func LoadToken(ctx context.Context, s *store.Store, userID uuid.UUID, key []byte) (*oauth2.Token, error) {
	rec, err := s.GetOAuthToken(ctx, userID, "google")
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	return decryptToken(rec.AccessCT, rec.RefreshCT, rec.ExpiresAt, key)
}

func SaveToken(ctx context.Context, s *store.Store, userID uuid.UUID, key []byte, tok *oauth2.Token) error {
	accessCT, refreshCT, err := encryptToken(tok, key)
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return s.SaveOAuthToken(ctx, store.OAuthToken{
		UserID:    userID,
		Provider:  "google",
		AccessCT:  accessCT,
		RefreshCT: refreshCT,
		ExpiresAt: tok.Expiry,
	})
}

func encryptToken(tok *oauth2.Token, key []byte) (access, refresh []byte, err error) {
	access, err = oauth.Encrypt([]byte(tok.AccessToken), key)
	if err != nil {
		return nil, nil, err
	}
	refresh, err = oauth.Encrypt([]byte(tok.RefreshToken), key)
	if err != nil {
		return nil, nil, err
	}
	return access, refresh, nil
}

func decryptToken(access, refresh []byte, expiry time.Time, key []byte) (*oauth2.Token, error) {
	a, err := oauth.Decrypt(access, key)
	if err != nil {
		return nil, fmt.Errorf("decrypt access: %w", err)
	}
	r, err := oauth.Decrypt(refresh, key)
	if err != nil {
		return nil, fmt.Errorf("decrypt refresh: %w", err)
	}
	return &oauth2.Token{
		AccessToken:  string(a),
		RefreshToken: string(r),
		TokenType:    "Bearer",
		Expiry:       expiry,
	}, nil
}

// savingSource wraps a base TokenSource and calls save whenever Token() returns
// a different value than the previous one. The "previous" token is seeded at
// construction so that a refreshed token (different access string) triggers save.
type savingSource struct {
	base oauth2.TokenSource
	save func(*oauth2.Token) error
	last *oauth2.Token
}

func newSavingSource(base oauth2.TokenSource, save func(*oauth2.Token) error, seed *oauth2.Token) oauth2.TokenSource {
	return &savingSource{base: base, save: save, last: seed}
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if s.last == nil || tok.AccessToken != s.last.AccessToken {
		if err := s.save(tok); err != nil {
			return nil, fmt.Errorf("save: %w", err)
		}
		s.last = tok
	}
	return tok, nil
}
```

Run tests — expect 3 pass (savingSource × 2 + roundtrip).

Commit: `feat(gmail): add LoadToken / SaveToken / savingSource helpers`

---

## Section C — Client.New + LatestMessageIDs (TDD with httptest)

### Task C1: Failing test

**Files:**
- Create: `internal/gmail/client_integration_test.go`

- [ ] **Step 1**

```go
//go:build integration

package gmail_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

// gmailMock builds an httptest.Server with handlers for the History.List and
// Messages.Get endpoints. Each test case provides the JSON to return.
type gmailMock struct {
	historyJSON string
	messageJSON string
	historyHits int
	messageHits int
}

func (m *gmailMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/history", func(w http.ResponseWriter, _ *http.Request) {
		m.historyHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.historyJSON))
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/", func(w http.ResponseWriter, _ *http.Request) {
		m.messageHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.messageJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newKey32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func setupClient(t *testing.T, m *gmailMock) (*gmail.Client, *store.Store) {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)

	u, err := s.CreateUser(context.Background(), fmt.Sprintf("test+%d@example.com", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}

	key := newKey32()
	tok := &oauth2.Token{
		AccessToken:  "fake-access",
		RefreshToken: "fake-refresh",
		Expiry:       time.Now().Add(1 * time.Hour),
	}
	if err := gmail.SaveToken(context.Background(), s, u.ID, key, tok); err != nil {
		t.Fatal(err)
	}

	srv := m.server(t)
	cfg := &oauth2.Config{ClientID: "x", ClientSecret: "y"}
	c, err := gmail.New(context.Background(), gmail.Config{
		Store:         s,
		UserID:        u.ID,
		EncryptionKey: key,
		OAuth2:        cfg,
		Endpoint:      srv.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c, s
}

func TestLatestMessageIDs_ReturnsInboxPrimaryAdds(t *testing.T) {
	histResp := map[string]any{
		"history": []map[string]any{
			{
				"id": "100",
				"messagesAdded": []map[string]any{
					{"message": map[string]any{"id": "m1", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
					{"message": map[string]any{"id": "m2", "labelIds": []string{"INBOX", "CATEGORY_PROMOTIONS"}}},
					{"message": map[string]any{"id": "m3", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
				},
			},
		},
		"historyId": "150",
	}
	b, _ := json.Marshal(histResp)

	c, _ := setupClient(t, &gmailMock{historyJSON: string(b)})

	ids, cursor, err := c.LatestMessageIDs(context.Background(), "1")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m3" {
		t.Fatalf("ids: %v, want [m1 m3]", ids)
	}
	if cursor != "150" {
		t.Fatalf("cursor: %q, want 150", cursor)
	}
}

func TestLatestMessageIDs_EmptyHistory(t *testing.T) {
	histResp := map[string]any{"historyId": "200"}
	b, _ := json.Marshal(histResp)

	c, _ := setupClient(t, &gmailMock{historyJSON: string(b)})

	ids, cursor, err := c.LatestMessageIDs(context.Background(), "1")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids: %v, want empty", ids)
	}
	if cursor != "200" {
		t.Fatalf("cursor: %q, want 200", cursor)
	}
}
```

Run `go test -tags=integration ./internal/gmail/...` — expect undefined `gmail.Client`, `gmail.Config`, `gmail.New`, `c.LatestMessageIDs`. Capture last 3 lines.

### Task C2: Implement Client + LatestMessageIDs

**Files:**
- Create: `internal/gmail/client.go`

- [ ] **Step 1**

```go
package gmail

import (
	"context"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Store         *store.Store
	UserID        uuid.UUID
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	Endpoint      string // empty -> Google's default endpoint
}

type Client struct {
	svc    *gmailapi.Service
	store  *store.Store
	userID uuid.UUID
	key    []byte
}

func New(ctx context.Context, cfg Config) (*Client, error) {
	tok, err := LoadToken(ctx, cfg.Store, cfg.UserID, cfg.EncryptionKey)
	if err != nil {
		return nil, err
	}

	base := cfg.OAuth2.TokenSource(ctx, tok)
	saving := newSavingSource(base, func(t *oauth2.Token) error {
		return SaveToken(ctx, cfg.Store, cfg.UserID, cfg.EncryptionKey, t)
	}, tok)
	httpClient := oauth2.NewClient(ctx, saving)

	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if cfg.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(cfg.Endpoint))
	}
	svc, err := gmailapi.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gmail svc: %w", err)
	}
	return &Client{svc: svc, store: cfg.Store, userID: cfg.UserID, key: cfg.EncryptionKey}, nil
}

func (c *Client) LatestMessageIDs(ctx context.Context, lastHistoryID string) (newIDs []string, newCursor string, err error) {
	startID, err := strconv.ParseUint(lastHistoryID, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("parse history id %q: %w", lastHistoryID, err)
	}

	resp, err := c.svc.Users.History.List("me").
		StartHistoryId(startID).
		HistoryTypes("messageAdded").
		LabelId("INBOX").
		Context(ctx).
		Do()
	if err != nil {
		return nil, "", fmt.Errorf("history list: %w", err)
	}

	for _, h := range resp.History {
		for _, ma := range h.MessagesAdded {
			if ma.Message == nil {
				continue
			}
			if !hasLabel(ma.Message.LabelIds, "CATEGORY_PERSONAL") {
				continue
			}
			newIDs = append(newIDs, ma.Message.Id)
		}
	}

	cursor := lastHistoryID
	if resp.HistoryId != 0 {
		cursor = strconv.FormatUint(resp.HistoryId, 10)
	}
	return newIDs, cursor, nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
```

Run integration tests — expect 2 pass.

Commit: `feat(gmail): add Client.New and LatestMessageIDs via History API`

---

## Section D — FetchMessage (TDD with httptest)

### Task D1: Failing test

- [ ] **Step 1: Append to client_integration_test.go**

```go
func TestFetchMessage_DecodesRawBase64(t *testing.T) {
	rawMime := "From: alice@example.com\r\nSubject: hi\r\n\r\nbody"
	// gmail API uses base64url (RFC 4648 §5)
	rawB64 := base64URL(rawMime)
	msgResp := map[string]any{"raw": rawB64}
	b, _ := json.Marshal(msgResp)

	c, _ := setupClient(t, &gmailMock{messageJSON: string(b)})

	got, err := c.FetchMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(got) != rawMime {
		t.Fatalf("got %q, want %q", got, rawMime)
	}
}

func base64URL(s string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
}
```

You'll need to add `"encoding/base64"` to the imports.

Run — expect undefined `c.FetchMessage`. Capture.

### Task D2: Implement FetchMessage

- [ ] **Step 1: Append to client.go**

```go
import (
	"encoding/base64"
	// existing imports plus encoding/base64
)

func (c *Client) FetchMessage(ctx context.Context, messageID string) ([]byte, error) {
	msg, err := c.svc.Users.Messages.Get("me", messageID).Format("raw").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get message %s: %w", messageID, err)
	}
	raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(msg.Raw)
	if err != nil {
		// fall back to standard padded variant just in case
		raw, err = base64.URLEncoding.DecodeString(msg.Raw)
		if err != nil {
			return nil, fmt.Errorf("decode raw: %w", err)
		}
	}
	return raw, nil
}
```

Consolidate the imports.

Run — expect 3 integration tests pass total.

Commit: `feat(gmail): add FetchMessage with base64url MIME decoding`

---

## Section E — Local sweep + PR + tag

### Task E1: Sweep

```bash
make lint
make test-unit
make test-integration
make k8s-validate
make loadtest
```

All exit 0.

### Task E2: PR + merge

```bash
git push -u origin phase-3b-gmail-client

gh pr create --base main --head phase-3b-gmail-client \
  --title "feat(gmail): Phase 3B — Gmail client (LatestMessageIDs + FetchMessage)" \
  --body "$(cat <<'EOF'
## Summary

Phase 3B: the Gmail-side I/O the workers need.

- `internal/gmail/token.go` — `LoadToken` / `SaveToken` (encrypted at rest via Phase 3A's oauth helpers) plus `savingSource`, an `oauth2.TokenSource` wrapper that auto-persists refreshed tokens back to Postgres.
- `internal/gmail/client.go` — `New(ctx, Config)` constructs an authenticated `*gmail.Service` (configurable endpoint for tests). `LatestMessageIDs(ctx, lastHistoryID)` walks `Users.History.List` and returns INBOX/PRIMARY message IDs plus the new cursor. `FetchMessage(ctx, id)` returns raw MIME bytes via `Users.Messages.Get(id).Format("raw")`.
- All HTTP exchanges with Gmail mocked via `httptest.Server` — no real Google calls in tests. Manual real-OAuth setup ships in Phase 3F.

3 unit tests + 3 integration tests. Existing 33 tests still green.

Phase 3C (Gotenberg PDF rendering) builds on top.

## Test plan

- [x] make lint
- [x] make test-unit
- [x] make test-integration
- [x] make k8s-validate
- [x] make loadtest
- [ ] CI green
EOF
)"

PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3b -m "Phase 3B: Gmail client (LatestMessageIDs + FetchMessage)"
git push origin phase-3b
gh release create phase-3b --title "Phase 3B: Gmail Client" \
  --notes "Authenticated gmail.Service with auto-saving token refresh. LatestMessageIDs (History API → INBOX/PRIMARY message IDs + cursor). FetchMessage (raw MIME via base64url). httptest-mocked tests; real OAuth flow lands in Phase 3F."
git push origin --delete phase-3b-gmail-client
git branch -D phase-3b-gmail-client
```

---

## Out of scope

- Real OAuth flow / first sync (Phase 3F)
- Gotenberg PDF rendering (Phase 3C)
- Drive client (Phase 3D)
- Stage handlers wiring this together (Phase 3E)
- Scheduler binary that calls LatestMessageIDs on a tick (Phase 3F)
