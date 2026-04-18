# Phase 3F — Scheduler + OAuth Setup

> Use `superpowers:subagent-driven-development`. `- [ ]` checkboxes track steps.

**Goal:** Close out Phase 3 by adding the two pieces that let the user actually run a real Gmail→Drive sync end-to-end:

1. `cmd/scheduler` — polls Gmail History every 5 min for each user, enqueues `fetch` jobs for new INBOX/PRIMARY messages, advances the cursor.
2. `cmd/oauth-setup` — interactive helper that walks the Google OAuth flow once, encrypts and saves the token to Postgres.

Plus README docs for getting from clone to running sync.

**Architecture:**

```
internal/store/  (appended)
  gmail_sync.go     ListUsers / GetGmailSyncState / SetGmailSyncState
internal/scheduler/
  scheduler.go      Scheduler + Run(ctx) + PollOnce(ctx)
  scheduler_integration_test.go    real PG + Redis + httptest gmail
cmd/scheduler/main.go
cmd/oauth-setup/main.go
README.md           (modified) Setup section
```

**Branch:** `phase-3f-scheduler-oauth`.

---

## Section A — Branch + sync-state store methods

### Task A1: Branch

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3f-scheduler-oauth
```

### Task A2: Failing test

Append to `internal/store/store_integration_test.go`:

```go
func TestListUsers_ReturnsAllUsers(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	_, _ = s.CreateUser(ctx, "a@example.com")
	_, _ = s.CreateUser(ctx, "b@example.com")

	users, err := s.ListUsers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(users) < 2 {
		t.Fatalf("got %d users, want >=2", len(users))
	}
}

func TestGmailSyncState_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "sync@example.com")

	got, err := s.GetGmailSyncState(ctx, u.ID)
	if err != nil {
		t.Fatalf("get empty: %v", err)
	}
	if got != "" {
		t.Fatalf("empty state: got %q, want \"\"", got)
	}

	if err := s.SetGmailSyncState(ctx, u.ID, "12345"); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, _ = s.GetGmailSyncState(ctx, u.ID)
	if got != "12345" {
		t.Fatalf("got %q, want 12345", got)
	}

	if err := s.SetGmailSyncState(ctx, u.ID, "67890"); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got, _ = s.GetGmailSyncState(ctx, u.ID)
	if got != "67890" {
		t.Fatalf("got %q, want 67890", got)
	}
}
```

Run — expect undefined `s.ListUsers`, `s.GetGmailSyncState`, `s.SetGmailSyncState`. Capture.

### Task A3: Implement

Create `internal/store/gmail_sync.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	const q = `SELECT id, email, created_at FROM users ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// GetGmailSyncState returns "" if no row exists for this user.
func (s *Store) GetGmailSyncState(ctx context.Context, userID uuid.UUID) (string, error) {
	const q = `SELECT history_id FROM gmail_sync_state WHERE user_id = $1`

	var hid string
	err := s.pool.QueryRow(ctx, q, userID).Scan(&hid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get sync state: %w", err)
	}
	return hid, nil
}

func (s *Store) SetGmailSyncState(ctx context.Context, userID uuid.UUID, historyID string) error {
	const q = `
		INSERT INTO gmail_sync_state (user_id, history_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE
		SET history_id = EXCLUDED.history_id, updated_at = now()`

	if _, err := s.pool.Exec(ctx, q, userID, historyID); err != nil {
		return fmt.Errorf("set sync state: %w", err)
	}
	return nil
}
```

Run — expect 21 store integration tests pass total (existing 18 + 3 new).

Commit: `feat(store): add ListUsers, GetGmailSyncState, SetGmailSyncState`

---

## Section B — Scheduler

### Task B1: Failing test

Create `internal/scheduler/scheduler_integration_test.go`:

```go
//go:build integration

package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/scheduler"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func newKey32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func TestPollOnce_EnqueuesNewMessageJobs(t *testing.T) {
	histResp, _ := json.Marshal(map[string]any{
		"history": []map[string]any{
			{
				"id": "100",
				"messagesAdded": []map[string]any{
					{"message": map[string]any{"id": "msg-1", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
					{"message": map[string]any{"id": "msg-2", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
				},
			},
		},
		"historyId": "150",
	})
	profileResp, _ := json.Marshal(map[string]any{"historyId": "10"})

	gmailMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/gmail/v1/users/me/profile":
			_, _ = w.Write(profileResp)
		case r.URL.Path == "/gmail/v1/users/me/history":
			_, _ = w.Write(histResp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gmailMock.Close()

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("sched+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}
	// Seed sync state to skip the initial-sync GetProfile path on the FIRST poll.
	if err := s.SetGmailSyncState(context.Background(), u.ID, "1"); err != nil {
		t.Fatal(err)
	}

	sch := scheduler.New(scheduler.Config{
		Store:         s,
		Queue:         q,
		EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: gmailMock.URL,
	})

	if err := sch.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 2 {
		t.Fatalf("queue depth: got %d, want 2", depth)
	}

	cursor, _ := s.GetGmailSyncState(context.Background(), u.ID)
	if cursor != "150" {
		t.Fatalf("cursor: got %q, want 150", cursor)
	}
}

func TestPollOnce_InitializesSyncStateFromProfile(t *testing.T) {
	profileResp, _ := json.Marshal(map[string]any{"historyId": "999"})

	gmailMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			_, _ = w.Write(profileResp)
			return
		}
		http.NotFound(w, r)
	}))
	defer gmailMock.Close()

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("init+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	_ = oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok)

	sch := scheduler.New(scheduler.Config{
		Store: s, Queue: q, EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: gmailMock.URL,
	})

	if err := sch.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	cursor, _ := s.GetGmailSyncState(context.Background(), u.ID)
	if cursor != "999" {
		t.Fatalf("cursor after init: got %q, want 999", cursor)
	}
	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 0 {
		t.Fatalf("expected no jobs on init, got %d", depth)
	}
}
```

Run — expect undefined `scheduler.New`, etc. Capture.

### Task B2: Implement scheduler

Create `internal/scheduler/scheduler.go`:

```go
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Store         *store.Store
	Queue         *queue.Queue
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	GmailEndpoint string
	Interval      time.Duration
}

type Scheduler struct {
	cfg Config
}

func New(cfg Config) *Scheduler {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	return &Scheduler{cfg: cfg}
}

func (s *Scheduler) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()

	if err := s.PollOnce(ctx); err != nil {
		slog.Error("initial poll", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := s.PollOnce(ctx); err != nil {
				slog.Error("poll", "err", err)
			}
		}
	}
}

func (s *Scheduler) PollOnce(ctx context.Context) error {
	users, err := s.cfg.Store.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if err := s.pollUser(ctx, u); err != nil {
			slog.Warn("poll user failed", "user_id", u.ID, "err", err)
		}
	}
	return nil
}

func (s *Scheduler) pollUser(ctx context.Context, u store.User) error {
	cursor, err := s.cfg.Store.GetGmailSyncState(ctx, u.ID)
	if err != nil {
		return err
	}

	if cursor == "" {
		// First poll for this user — seed cursor from current profile, no
		// backfill (forward-sync only per spec).
		hid, err := s.fetchInitialHistoryID(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("init history id: %w", err)
		}
		if err := s.cfg.Store.SetGmailSyncState(ctx, u.ID, hid); err != nil {
			return fmt.Errorf("save init cursor: %w", err)
		}
		slog.Info("initialized sync cursor", "user_id", u.ID, "history_id", hid)
		return nil
	}

	client, err := gmail.New(ctx, gmail.Config{
		Store:         s.cfg.Store,
		UserID:        u.ID,
		EncryptionKey: s.cfg.EncryptionKey,
		OAuth2:        s.cfg.OAuth2,
		Endpoint:      s.cfg.GmailEndpoint,
	})
	if err != nil {
		return fmt.Errorf("gmail client: %w", err)
	}

	ids, newCursor, err := client.LatestMessageIDs(ctx, cursor)
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}

	for _, msgID := range ids {
		gid := msgID
		uid := u.ID
		job, err := s.cfg.Store.EnqueueJob(ctx, store.NewJob{
			Stage:          "fetch",
			UserID:         &uid,
			GmailMessageID: &gid,
			Payload:        json.RawMessage(`{}`),
		})
		if err != nil {
			slog.Warn("enqueue failed", "msg_id", gid, "err", err)
			continue
		}
		if err := s.cfg.Queue.Push(ctx, "fetch", job.ID.String()); err != nil {
			slog.Warn("push failed", "job_id", job.ID, "err", err)
			continue
		}
		slog.Info("enqueued fetch", "job_id", job.ID, "msg_id", gid)
	}

	if newCursor != cursor {
		if err := s.cfg.Store.SetGmailSyncState(ctx, u.ID, newCursor); err != nil {
			return fmt.Errorf("save cursor: %w", err)
		}
	}
	return nil
}

// fetchInitialHistoryID does NOT use the gmail.Client wrapper because we don't
// want to load+save tokens for a profile-only call. Construct the service
// directly using the same OAuth setup.
func (s *Scheduler) fetchInitialHistoryID(ctx context.Context, userID uuid.UUID) (string, error) {
	tok, err := oauth.LoadToken(ctx, s.cfg.Store, userID, s.cfg.EncryptionKey, "google")
	if err != nil {
		return "", err
	}
	base := s.cfg.OAuth2.TokenSource(ctx, tok)
	saving := oauth.NewSavingSource(base, func(t *oauth2.Token) error {
		return oauth.SaveToken(ctx, s.cfg.Store, userID, s.cfg.EncryptionKey, "google", t)
	}, tok)
	httpClient := oauth2.NewClient(ctx, saving)

	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if s.cfg.GmailEndpoint != "" {
		opts = append(opts, option.WithEndpoint(s.cfg.GmailEndpoint))
	}
	svc, err := gmailapi.NewService(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("gmail svc: %w", err)
	}
	prof, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get profile: %w", err)
	}
	return fmt.Sprintf("%d", prof.HistoryId), nil
}
```

(You'll need to add `"github.com/google/uuid"` to imports.)

Run integration tests — expect 2 pass.

Commit: `feat(scheduler): add Scheduler with PollOnce (initial-sync + history poll)`

### Task B3: cmd/scheduler

Create `cmd/scheduler/main.go`:

```go
package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/scheduler"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dsn := envOr("DATABASE_URL", "postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")
	clientID := mustEnv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := mustEnv("GOOGLE_OAUTH_CLIENT_SECRET")
	keyB64 := mustEnv("TOKEN_ENCRYPTION_KEY")

	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		slog.Error("TOKEN_ENCRYPTION_KEY must decode to 32 bytes", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, dsn); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("pg connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		slog.Error("redis url", "err", err)
		os.Exit(1)
	}
	redis := goredis.NewClient(opts)
	defer func() { _ = redis.Close() }()

	sch := scheduler.New(scheduler.Config{
		Store:         store.New(pool),
		Queue:         queue.New(redis),
		EncryptionKey: key,
		OAuth2: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
		},
		Interval: 5 * time.Minute,
	})

	slog.Info("scheduler starting")
	if err := sch.Run(ctx); err != nil {
		slog.Error("scheduler run", "err", err)
		os.Exit(1)
	}
	slog.Info("scheduler stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing env var", "key", k)
		os.Exit(1)
	}
	return v
}
```

Build: `go build ./cmd/scheduler` — expect silent success.

Commit: `feat(scheduler): add cmd/scheduler binary with 5 min poll loop`

---

## Section C — cmd/oauth-setup

Create `cmd/oauth-setup/main.go`:

```go
// Package main is a one-off helper to populate the oauth_tokens table with a
// real Google OAuth token for a given email. Run once per user.
//
// Usage:
//   ./oauth-setup --email=alice@example.com
//
// Environment variables required:
//   DATABASE_URL              postgres connection string
//   GOOGLE_OAUTH_CLIENT_ID    from Google Cloud OAuth 2.0 Client IDs page
//   GOOGLE_OAUTH_CLIENT_SECRET
//   TOKEN_ENCRYPTION_KEY      base64 of 32 random bytes (also used by workers)
//
// The redirect URI configured in Google Cloud must include http://localhost:8888/callback.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

const callbackPort = "8888"

func main() {
	email := flag.String("email", "", "email address (must match the Google account you'll authorize)")
	flag.Parse()
	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email is required")
		os.Exit(2)
	}

	dsn := mustEnv("DATABASE_URL")
	clientID := mustEnv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := mustEnv("GOOGLE_OAUTH_CLIENT_SECRET")
	keyB64 := mustEnv("TOKEN_ENCRYPTION_KEY")

	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		log.Fatalf("TOKEN_ENCRYPTION_KEY must decode to 32 bytes: %v", err)
	}

	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pg connect: %v", err)
	}
	defer pool.Close()
	s := store.New(pool)

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  "http://localhost:" + callbackPort + "/callback",
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
		Endpoint:     google.Endpoint,
	}

	state := fmt.Sprintf("dtq-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	fmt.Println("Open this URL in your browser:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("Authorize the app for " + *email + " and complete the redirect to localhost.")
	fmt.Println()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{
		Addr:              ":" + callbackPort,
		ReadHeaderTimeout: 5 * time.Second,
	}
	http.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- errors.New("state mismatch")
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- errors.New("no code in callback")
			http.Error(w, "no code", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("OK. You can close this tab and return to the terminal."))
		codeCh <- code
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		log.Fatalf("callback: %v", err)
	case <-time.After(5 * time.Minute):
		log.Fatal("timed out waiting for callback")
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		log.Fatalf("exchange: %v", err)
	}

	user, err := s.GetUserByEmail(ctx, *email)
	if errors.Is(err, store.ErrUserNotFound) {
		user, err = s.CreateUser(ctx, *email)
		if err != nil {
			log.Fatalf("create user: %v", err)
		}
		fmt.Println("Created user:", user.ID)
	} else if err != nil {
		log.Fatalf("lookup user: %v", err)
	} else {
		fmt.Println("Existing user:", user.ID)
	}

	if err := oauth.SaveToken(ctx, s, user.ID, key, "google", tok); err != nil {
		log.Fatalf("save token: %v", err)
	}
	fmt.Println("Token saved for user", user.ID, "(", *email, "). Scheduler can now sync this account.")
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env var: %s", k)
	}
	return v
}
```

Build: `go build ./cmd/oauth-setup` — expect silent success.

Commit: `feat(oauth-setup): add cmd/oauth-setup helper for one-time token bootstrap`

---

## Section D — README setup section

Append to `README.md` after the Quickstart section:

```markdown
## Real Gmail → Drive setup (one-time)

If you want to actually run the Gmail-to-Drive sync (rather than just synthetic
loadtest jobs), you need to set up a Google Cloud OAuth client and bootstrap a
token. About 10 minutes of clicking.

1. **Google Cloud Console:** Create (or reuse) a project. Enable the **Gmail API**
   and **Drive API**. On the OAuth consent screen, set the scopes to
   `gmail.readonly` and `drive.file`. Add your email as a test user.

2. **OAuth client:** Create an OAuth 2.0 Client ID, type **Desktop**.
   Add `http://localhost:8888/callback` as an authorized redirect URI.
   Note the client ID and secret.

3. **Encryption key:** Generate a 32-byte random key:

   ```bash
   openssl rand -base64 32
   ```

   Save it; the same value is needed by both `oauth-setup` and the workers/scheduler.

4. **Bring up the local stack:**

   ```bash
   make docker-up
   ```

5. **Bootstrap your OAuth token:**

   ```bash
   export DATABASE_URL='postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable'
   export GOOGLE_OAUTH_CLIENT_ID='...'
   export GOOGLE_OAUTH_CLIENT_SECRET='...'
   export TOKEN_ENCRYPTION_KEY='<the base64 key>'
   go run ./cmd/oauth-setup --email=you@example.com
   ```

   Open the printed URL in a browser, authorize the app, the helper exits with
   "Token saved".

6. **Run the workers + scheduler:**

   ```bash
   # in one terminal each, or via docker compose:
   ./worker --stage=fetch
   ./worker --stage=render
   ./worker --stage=upload
   ./scheduler
   ```

   On the first poll the scheduler initializes your cursor from your current
   Gmail history ID (no backfill of existing inbox). Subsequent polls pick up
   new INBOX/PRIMARY messages and push them through the fetch → render → upload
   pipeline.

7. **Where do the PDFs land?** A new folder tree at the root of your Drive:
   `<root>/YYYY/MM/DD/`. Set `DRIVE_ROOT_FOLDER_ID` to the Drive folder ID where
   you want this tree to live (or leave unset to land at the root).
```

Commit: `docs: add real Gmail->Drive setup walkthrough to README`

---

## Section E — Local sweep + PR

```bash
make lint && make test-unit && make test-integration && make k8s-validate && make loadtest
```

All exit 0.

```bash
git push -u origin phase-3f-scheduler-oauth

gh pr create --base main --head phase-3f-scheduler-oauth \
  --title "feat(scheduler,oauth-setup): Phase 3F — Scheduler + OAuth bootstrap helper" \
  --body "Phase 3F closes Phase 3.

- internal/scheduler — Scheduler.PollOnce iterates users, on first sync seeds cursor from Users.GetProfile (forward-sync only), on subsequent polls calls gmail.LatestMessageIDs and enqueues fetch jobs.
- New store methods: ListUsers, GetGmailSyncState, SetGmailSyncState (UPSERT).
- cmd/scheduler — 5 min poll loop, signal-handled shutdown.
- cmd/oauth-setup — interactive helper that walks the Google OAuth dance once, listens on http://localhost:8888/callback, exchanges the code, encrypts and saves the token. Auto-creates the user row if needed.
- README: Real Gmail->Drive setup walkthrough.

Phase 3 is now complete. The full pipeline runs end-to-end with a real Google account."

PR=\$(gh pr view --json number -q .number)
gh pr merge \$PR --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3f -m "Phase 3F: Scheduler + OAuth bootstrap"
git push origin phase-3f
git tag -a phase-3 -m "Phase 3 milestone: Gmail -> PDF -> Drive pipeline complete"
git push origin phase-3

gh release create phase-3f --title "Phase 3F: Scheduler + OAuth Setup" \
  --notes "Scheduler binary that polls Gmail every 5 min and enqueues fetch jobs. Plus cmd/oauth-setup, a one-time helper that walks the Google OAuth flow and saves an encrypted token to Postgres."

gh release create phase-3 --title "Phase 3: Gmail → Drive Pipeline (Milestone)" \
  --notes "Phase 3 complete. The full Gmail → PDF → Drive pipeline runs end-to-end with a real Google account.

Sub-phases shipped: 3A (User identity + OAuth storage), 3B (Gmail client), 3C (Gotenberg PDF), 3D (Drive client + folder cache), 3E (Stage handlers), 3F (Scheduler + OAuth setup).

Next: Phase 1.5 (Hetzner k3s + multi-env CI/CD + first live deploy) and Phase 4 (live dashboard with B+D demo triggers)."

git push origin --delete phase-3f-scheduler-oauth
git branch -D phase-3f-scheduler-oauth
```
