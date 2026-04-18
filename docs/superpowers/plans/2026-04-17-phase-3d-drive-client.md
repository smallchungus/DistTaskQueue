# Phase 3D — Drive Client + Folder Cache

> Use `superpowers:subagent-driven-development`. `- [ ]` checkboxes track steps.

**Goal:** Build the Drive-side I/O for the upload worker: ensure folders exist (create if not), upload files, and cache folder IDs in Redis to avoid hammering the Drive API. First, refactor the OAuth token helpers out of `internal/gmail` and into `internal/oauth` so both Gmail and Drive can share them.

**Architecture:**

```
internal/oauth/
  token.go              (NEW) LoadToken / SaveToken / savingSource / encrypt+decrypt
  token_test.go         (NEW, moved from gmail)
  crypto.go             (existing, unchanged)
internal/gmail/
  token.go              (DELETED)
  token_test.go         (DELETED)
  client.go             (modified) imports oauth.LoadToken / oauth.SaveToken / oauth.NewSavingSource
internal/drive/
  client.go             Client struct, New(ctx, Config), EnsureFolder, Upload
  client_integration_test.go    httptest mocks Drive API; real Postgres for tokens
  cache.go              FolderCache wrapping a Redis client
  cache_integration_test.go     real Redis via testutil
```

The `oauth` package gets the canonical `LoadToken(ctx, store, userID, key, provider)` and `SaveToken(...)` (now generic — pass any provider name; "google" works for both Gmail and Drive since they share an OAuth token in our setup).

`drive.Client` constructor mirrors `gmail.Client` — load token via oauth, wrap with savingSource, build authenticated `*http.Client`, hand to `drive.NewService`.

`EnsureFolder(ctx, parentID, name) (folderID string, err error)`:
- Query: `drive.Files.List().Q("name='<name>' and '<parentID>' in parents and mimeType='application/vnd.google-apps.folder' and trashed=false")`
- If a result exists, return its ID. Otherwise create it via `Files.Create({Name: name, Parents: [parentID], MimeType: "application/vnd.google-apps.folder"})` and return the new ID.

`Upload(ctx, parentID, name, contentType string, content []byte) (fileID string, err error)`:
- `Files.Create({Name: name, Parents: [parentID]}).Media(bytes.NewReader(content), googleapi.ContentType(contentType))`
- Returns the file ID.

`FolderCache` is a thin wrapper over `*goredis.Client`:
- `Get(ctx, userID, path) (folderID string, ok bool)` — reads `drive_folder_cache:<userID>:<path>`
- `Set(ctx, userID, path, folderID, ttl) error`

Higher-level path resolution (date-tree `YYYY/MM/DD/<email-folder>/`) lands in Phase 3E with the upload handler.

**Scope:** uses `drive.file` scope only (covers what this app creates).

**Tech:** `google.golang.org/api/drive/v3`. OAuth/option already pulled in by Phase 3B.

**Branch:** `phase-3d-drive-client`.

---

## Section A — Branch + refactor token helpers to oauth package

### Task A1: Branch

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3d-drive-client
```

### Task A2: Move helpers to oauth pkg

- Create `internal/oauth/token.go` containing the contents of `internal/gmail/token.go` with:
  - Package name changed to `oauth`
  - Self-imports removed (no `oauth.Encrypt` — it's local now)
  - `encryptToken` and `decryptToken` renamed to `EncryptToken` / `DecryptToken` (exported, used by tests in oauth pkg)
  - `newSavingSource` renamed to `NewSavingSource` (exported, used by gmail and drive)
  - `LoadToken` signature: `LoadToken(ctx, s *store.Store, userID uuid.UUID, key []byte, provider string)` — adds `provider` arg (default to caller passing "google")
  - `SaveToken` signature: `SaveToken(ctx, s *store.Store, userID uuid.UUID, key []byte, provider string, tok *oauth2.Token)` — same

Final `internal/oauth/token.go`:

```go
package oauth

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

func LoadToken(ctx context.Context, s *store.Store, userID uuid.UUID, key []byte, provider string) (*oauth2.Token, error) {
	rec, err := s.GetOAuthToken(ctx, userID, provider)
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	return DecryptToken(rec.AccessCT, rec.RefreshCT, rec.ExpiresAt, key)
}

func SaveToken(ctx context.Context, s *store.Store, userID uuid.UUID, key []byte, provider string, tok *oauth2.Token) error {
	accessCT, refreshCT, err := EncryptToken(tok, key)
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return s.SaveOAuthToken(ctx, store.OAuthToken{
		UserID:    userID,
		Provider:  provider,
		AccessCT:  accessCT,
		RefreshCT: refreshCT,
		ExpiresAt: tok.Expiry,
	})
}

func EncryptToken(tok *oauth2.Token, key []byte) (access, refresh []byte, err error) {
	access, err = Encrypt([]byte(tok.AccessToken), key)
	if err != nil {
		return nil, nil, err
	}
	refresh, err = Encrypt([]byte(tok.RefreshToken), key)
	if err != nil {
		return nil, nil, err
	}
	return access, refresh, nil
}

func DecryptToken(access, refresh []byte, expiry time.Time, key []byte) (*oauth2.Token, error) {
	a, err := Decrypt(access, key)
	if err != nil {
		return nil, fmt.Errorf("decrypt access: %w", err)
	}
	r, err := Decrypt(refresh, key)
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

type savingSource struct {
	base oauth2.TokenSource
	save func(*oauth2.Token) error
	last *oauth2.Token
}

func NewSavingSource(base oauth2.TokenSource, save func(*oauth2.Token) error, seed *oauth2.Token) oauth2.TokenSource {
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

### Task A3: Move tests

Move `internal/gmail/token_test.go` → `internal/oauth/token_test.go` with these changes:
- Package name `oauth` (not `gmail`)
- `newSavingSource` → `NewSavingSource`
- `encryptToken` → `EncryptToken`, `decryptToken` → `DecryptToken`
- Drop the unused `_ = fakeStore{}`, `_ = uuid.UUID{}`, etc. lines if they exist

### Task A4: Update gmail/client.go to import from oauth

In `internal/gmail/client.go`:
- Import `"github.com/smallchungus/disttaskqueue/internal/oauth"` (might already be there)
- Replace `LoadToken(...)` calls with `oauth.LoadToken(..., "google")`
- Replace `SaveToken(...)` calls with `oauth.SaveToken(..., "google", ...)`
- Replace `newSavingSource(...)` with `oauth.NewSavingSource(...)`

### Task A5: Delete old gmail/token.go and gmail/token_test.go

```bash
rm internal/gmail/token.go internal/gmail/token_test.go
```

### Task A6: Verify everything still works

```bash
go test ./internal/oauth/...
go test -tags=integration ./internal/oauth/...
go test -tags=integration ./internal/gmail/...
make lint
```

All green.

Commit: `refactor(oauth): move token helpers from gmail to oauth pkg`

---

## Section B — Drive client (TDD with httptest)

### Task B1: Add Drive dependency

```bash
go get google.golang.org/api/drive/v3
go mod tidy
```

Commit: `chore(drive): add google api drive/v3 dep`

### Task B2: Failing test

Create `internal/drive/client_integration_test.go`:

```go
//go:build integration

package drive_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/drive"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

type driveMock struct {
	listResp   string
	createResp string
	uploadResp string
	listHits   int
	createHits int
	uploadHits int
	lastUpload []byte
}

func (m *driveMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	// Files.List
	mux.HandleFunc("/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			m.listHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.listResp))
		case http.MethodPost:
			// could be create-folder OR upload (multipart)
			if strings.Contains(r.URL.Path, "upload") {
				m.uploadHits++
				body, _ := io.ReadAll(r.Body)
				m.lastUpload = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(m.uploadResp))
			} else {
				m.createHits++
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(m.createResp))
			}
		}
	})
	// Upload endpoint
	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		m.uploadHits++
		body, _ := io.ReadAll(r.Body)
		m.lastUpload = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.uploadResp))
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

func setupClient(t *testing.T, m *driveMock) *drive.Client {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("drive+%d@example.com", time.Now().UnixNano()))

	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}

	srv := m.server(t)
	cfg := &oauth2.Config{ClientID: "x", ClientSecret: "y"}
	c, err := drive.New(context.Background(), drive.Config{
		Store:         s,
		UserID:        u.ID,
		EncryptionKey: key,
		OAuth2:        cfg,
		Endpoint:      srv.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestEnsureFolder_ReturnsExistingFolderID(t *testing.T) {
	listJSON, _ := json.Marshal(map[string]any{
		"files": []map[string]any{{"id": "existing-folder-id", "name": "myfolder"}},
	})
	c := setupClient(t, &driveMock{listResp: string(listJSON)})

	id, err := c.EnsureFolder(context.Background(), "parent-id", "myfolder")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "existing-folder-id" {
		t.Fatalf("got %q, want existing-folder-id", id)
	}
}

func TestEnsureFolder_CreatesIfMissing(t *testing.T) {
	listJSON, _ := json.Marshal(map[string]any{"files": []map[string]any{}})
	createJSON, _ := json.Marshal(map[string]any{"id": "new-folder-id", "name": "newfolder"})
	c := setupClient(t, &driveMock{listResp: string(listJSON), createResp: string(createJSON)})

	id, err := c.EnsureFolder(context.Background(), "parent-id", "newfolder")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "new-folder-id" {
		t.Fatalf("got %q, want new-folder-id", id)
	}
}

func TestUpload_PostsContent(t *testing.T) {
	uploadJSON, _ := json.Marshal(map[string]any{"id": "uploaded-file-id"})
	m := &driveMock{uploadResp: string(uploadJSON)}
	c := setupClient(t, m)

	id, err := c.Upload(context.Background(), "parent-id", "report.pdf", "application/pdf", []byte("PDF-DATA"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if id != "uploaded-file-id" {
		t.Fatalf("got %q, want uploaded-file-id", id)
	}
	if !contains(m.lastUpload, []byte("PDF-DATA")) {
		t.Fatalf("upload body did not contain PDF-DATA")
	}
}

func contains(haystack, needle []byte) bool {
	return strings.Contains(string(haystack), string(needle))
}
```

Run `go test -tags=integration ./internal/drive/...` — expect undefined `drive.Client`, `drive.Config`, `drive.New`, etc.

### Task B3: Implement Client

Create `internal/drive/client.go`:

```go
package drive

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	driveapi "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

const folderMime = "application/vnd.google-apps.folder"

type Config struct {
	Store         *store.Store
	UserID        uuid.UUID
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	Endpoint      string
}

type Client struct {
	svc *driveapi.Service
}

func New(ctx context.Context, cfg Config) (*Client, error) {
	tok, err := oauth.LoadToken(ctx, cfg.Store, cfg.UserID, cfg.EncryptionKey, "google")
	if err != nil {
		return nil, err
	}

	base := cfg.OAuth2.TokenSource(ctx, tok)
	saving := oauth.NewSavingSource(base, func(t *oauth2.Token) error {
		return oauth.SaveToken(ctx, cfg.Store, cfg.UserID, cfg.EncryptionKey, "google", t)
	}, tok)
	httpClient := oauth2.NewClient(ctx, saving)

	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if cfg.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(cfg.Endpoint))
	}
	svc, err := driveapi.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("drive svc: %w", err)
	}
	return &Client{svc: svc}, nil
}

func (c *Client) EnsureFolder(ctx context.Context, parentID, name string) (string, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false", escape(name), parentID, folderMime)
	resp, err := c.svc.Files.List().Q(q).Fields("files(id,name)").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("list folders: %w", err)
	}
	if len(resp.Files) > 0 {
		return resp.Files[0].Id, nil
	}

	created, err := c.svc.Files.Create(&driveapi.File{
		Name:     name,
		Parents:  []string{parentID},
		MimeType: folderMime,
	}).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create folder: %w", err)
	}
	return created.Id, nil
}

func (c *Client) Upload(ctx context.Context, parentID, name, contentType string, content []byte) (string, error) {
	created, err := c.svc.Files.Create(&driveapi.File{
		Name:    name,
		Parents: []string{parentID},
	}).Media(bytes.NewReader(content), googleapi.ContentType(contentType)).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return created.Id, nil
}

// escape escapes single quotes for Drive Q syntax: ' -> \'
func escape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\\', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}
```

Run integration tests — expect 3 pass.

Commit: `feat(drive): add Client with EnsureFolder and Upload`

---

## Section C — FolderCache (Redis-backed)

### Task C1: Failing test

Create `internal/drive/cache_integration_test.go`:

```go
//go:build integration

package drive_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smallchungus/disttaskqueue/internal/drive"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func TestFolderCache_SetThenGet(t *testing.T) {
	cli := testutil.StartRedis(t)
	cache := drive.NewFolderCache(cli)
	ctx := context.Background()
	uid := uuid.New()

	if err := cache.Set(ctx, uid, "2026/04/17", "folder-id-abc", time.Minute); err != nil {
		t.Fatal(err)
	}

	got, ok, err := cache.Get(ctx, uid, "2026/04/17")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "folder-id-abc" {
		t.Fatalf("got %q, ok=%v", got, ok)
	}
}

func TestFolderCache_GetReturnsFalseOnMiss(t *testing.T) {
	cli := testutil.StartRedis(t)
	cache := drive.NewFolderCache(cli)
	ctx := context.Background()
	uid := uuid.New()

	got, ok, err := cache.Get(ctx, uid, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok || got != "" {
		t.Fatalf("got %q, ok=%v, want miss", got, ok)
	}
}
```

Run — expect undefined `drive.NewFolderCache`, `drive.FolderCache`. Capture.

### Task C2: Implement FolderCache

Create `internal/drive/cache.go`:

```go
package drive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

type FolderCache struct {
	cli *goredis.Client
}

func NewFolderCache(cli *goredis.Client) *FolderCache {
	return &FolderCache{cli: cli}
}

func (c *FolderCache) Get(ctx context.Context, userID uuid.UUID, path string) (string, bool, error) {
	key := fmt.Sprintf("drive_folder_cache:%s:%s", userID, path)
	val, err := c.cli.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cache get: %w", err)
	}
	return val, true, nil
}

func (c *FolderCache) Set(ctx context.Context, userID uuid.UUID, path, folderID string, ttl time.Duration) error {
	key := fmt.Sprintf("drive_folder_cache:%s:%s", userID, path)
	if err := c.cli.Set(ctx, key, folderID, ttl).Err(); err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}
```

Run integration tests — expect 5 pass total in drive pkg.

Commit: `feat(drive): add Redis-backed FolderCache`

---

## Section D — Local sweep + PR

```bash
make lint && make test-unit && make test-integration && make k8s-validate && make loadtest
```

All exit 0.

```bash
git push -u origin phase-3d-drive-client
gh pr create --base main --head phase-3d-drive-client \
  --title "feat(drive): Phase 3D — Drive client + folder cache" \
  --body "Phase 3D: Drive-side I/O + small refactor.

- Refactor: token helpers (LoadToken, SaveToken, NewSavingSource, EncryptToken/DecryptToken) move from internal/gmail to internal/oauth so both Gmail and Drive consume them. The 'provider' arg is now a parameter (default 'google').
- internal/drive/client.go — Client.New, EnsureFolder (looks up by name+parent, creates if missing), Upload (simple multipart).
- internal/drive/cache.go — FolderCache wrapping Redis (drive_folder_cache:<userID>:<path> -> folderID), with Set/Get + TTL.
- 3 integration tests for Drive client (httptest mocks Drive API) + 2 for FolderCache (real Redis via testutil).

Phase 3E (handlers) wires this together with Gmail + PDF for the actual fetch->render->upload pipeline."

PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3d -m "Phase 3D: Drive client + folder cache"
git push origin phase-3d
gh release create phase-3d --title "Phase 3D: Drive Client + Folder Cache" \
  --notes "Drive client (EnsureFolder + Upload), Redis-backed FolderCache, and a small refactor moving OAuth token helpers from gmail to oauth pkg so both Gmail and Drive share them."
git push origin --delete phase-3d-drive-client
git branch -D phase-3d-drive-client
```
