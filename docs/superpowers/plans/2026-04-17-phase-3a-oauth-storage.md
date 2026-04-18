# Phase 3A — User Identity + OAuth Token Storage

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Lay the multi-tenant foundation for Gmail and Drive integration. Add the user / OAuth / sync-state / processed-email tables to Postgres, plus an AES-GCM helper for encrypting OAuth tokens at rest. No Google API code yet — just the storage primitives that Phase 3B (Gmail) and Phase 3D (Drive) will both lean on.

**Architecture:**

```
internal/store/migrations/
  0002_users_oauth_sync_processed.up.sql      adds tables + ALTERs pipeline_jobs
  0002_users_oauth_sync_processed.down.sql    reverses

internal/oauth/
  crypto.go             Encrypt/Decrypt(plaintext, key) — AES-GCM 256
  crypto_test.go        round-trip, key-length, tamper detection (pure unit)

internal/store/  (appended)
  users.go              CreateUser / GetUserByEmail / ErrUserNotFound
  oauth.go              SaveOAuthToken / GetOAuthToken / ErrOAuthTokenNotFound
  store_integration_test.go  TDD coverage for above
```

The `oauth_tokens` table stores ciphertext only — never plaintext access or refresh tokens. The encryption key lives in a k8s Secret (`TOKEN_ENCRYPTION_KEY`, 32-byte base64) at deploy time. For tests, the helper accepts an arbitrary 32-byte key the test generates.

`pipeline_jobs` gets ALTERed to add `user_id UUID NULL`, `gmail_message_id TEXT NULL`, `is_synthetic BOOLEAN NOT NULL DEFAULT false`. Plus the partial unique index `(user_id, gmail_message_id)` for non-terminal non-synthetic rows. Nullable so existing Phase 2 rows (synthetic test jobs) survive the migration.

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` Section 5.1.

**Branch:** `phase-3a-oauth-storage` → PR to `main`.

---

## Section A — Branch + migration

### Task A1: Branch

- [ ] **Step 1**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3a-oauth-storage
```

### Task A2: Up migration

**Files:**
- Create: `internal/store/migrations/0002_users_oauth_sync_processed.up.sql`

- [ ] **Step 1**

```sql
CREATE TABLE users (
  id            UUID PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE oauth_tokens (
  user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  provider      TEXT NOT NULL,
  access_ct     BYTEA NOT NULL,
  refresh_ct    BYTEA NOT NULL,
  expires_at    TIMESTAMPTZ NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, provider)
);

CREATE TABLE gmail_sync_state (
  user_id       UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  history_id    TEXT NOT NULL,
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE processed_emails (
  user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  gmail_message_id  TEXT NOT NULL,
  drive_folder_id   TEXT NOT NULL,
  processed_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, gmail_message_id)
);

ALTER TABLE pipeline_jobs
  ADD COLUMN user_id           UUID NULL REFERENCES users(id) ON DELETE SET NULL,
  ADD COLUMN gmail_message_id  TEXT NULL,
  ADD COLUMN is_synthetic      BOOLEAN NOT NULL DEFAULT false;

CREATE INDEX pipeline_jobs_user_stage_status_idx ON pipeline_jobs (user_id, stage, status);

CREATE UNIQUE INDEX pipeline_jobs_user_message_unique
  ON pipeline_jobs (user_id, gmail_message_id)
  WHERE status NOT IN ('done', 'dead') AND is_synthetic = false AND user_id IS NOT NULL AND gmail_message_id IS NOT NULL;
```

### Task A3: Down migration

**Files:**
- Create: `internal/store/migrations/0002_users_oauth_sync_processed.down.sql`

- [ ] **Step 1**

```sql
DROP INDEX IF EXISTS pipeline_jobs_user_message_unique;
DROP INDEX IF EXISTS pipeline_jobs_user_stage_status_idx;

ALTER TABLE pipeline_jobs
  DROP COLUMN IF EXISTS is_synthetic,
  DROP COLUMN IF EXISTS gmail_message_id,
  DROP COLUMN IF EXISTS user_id;

DROP TABLE IF EXISTS processed_emails;
DROP TABLE IF EXISTS gmail_sync_state;
DROP TABLE IF EXISTS oauth_tokens;
DROP TABLE IF EXISTS users;
```

### Task A4: Verify migration applies

- [ ] **Step 1: Failing test for new tables**

Append to `internal/store/migrate_integration_test.go`:

```go
func TestMigrate_AppliesAllPhase3ATables(t *testing.T) {
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	wantTables := []string{"users", "oauth_tokens", "gmail_sync_state", "processed_emails", "pipeline_jobs", "job_status_history"}
	for _, table := range wantTables {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`, table,
		).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}

	wantCols := []string{"user_id", "gmail_message_id", "is_synthetic"}
	for _, col := range wantCols {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema='public' AND table_name='pipeline_jobs' AND column_name=$1`, col,
		).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("pipeline_jobs.%s missing", col)
		}
	}
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/store/...
```

Expected: 12 tests pass (11 existing + 1 new).

- [ ] **Step 3: Commit**

```bash
git add internal/store/migrations/ internal/store/migrate_integration_test.go
git commit -m "feat(store): add migration 0002 for users, oauth, sync state, processed_emails"
```

---

## Section B — AES-GCM crypto helpers (pure unit TDD)

### Task B1: Failing tests

**Files:**
- Create: `internal/oauth/crypto_test.go`

- [ ] **Step 1**

```go
package oauth

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func newKey(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

func TestEncryptDecrypt_RoundTrip(t *testing.T) {
	key := newKey(t)
	plaintext := []byte("hello-oauth-token")

	ct, err := Encrypt(plaintext, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext == plaintext")
	}

	pt, err := Decrypt(ct, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(pt, plaintext) {
		t.Fatalf("got %q, want %q", pt, plaintext)
	}
}

func TestEncrypt_RejectsKeyOfWrongLength(t *testing.T) {
	_, err := Encrypt([]byte("x"), make([]byte, 16))
	if err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestDecrypt_FailsOnTamperedCiphertext(t *testing.T) {
	key := newKey(t)
	ct, _ := Encrypt([]byte("tamper-me"), key)
	ct[len(ct)-1] ^= 0x01

	if _, err := Decrypt(ct, key); err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestEncrypt_NonDeterministic(t *testing.T) {
	key := newKey(t)
	a, _ := Encrypt([]byte("same"), key)
	b, _ := Encrypt([]byte("same"), key)
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of same plaintext produced same ciphertext (nonce reuse)")
	}
}
```

Run `go test ./internal/oauth/...` — expect undefined `Encrypt`, `Decrypt`. Capture last 3 lines.

### Task B2: Implement crypto

**Files:**
- Create: `internal/oauth/crypto.go`

- [ ] **Step 1**

```go
package oauth

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
)

const KeySize = 32

var ErrInvalidKey = errors.New("oauth: key must be 32 bytes")

func Encrypt(plaintext, key []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func Decrypt(ciphertext, key []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("gcm: %w", err)
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns {
		return nil, errors.New("oauth: ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	pt, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	return pt, nil
}
```

Run tests — expect 4 pass.

Commit: `feat(oauth): add AES-GCM Encrypt/Decrypt helpers for token storage`

---

## Section C — User and OAuth store methods (TDD integration)

### Task C1: Failing tests for user methods

**Files:**
- Modify: `internal/store/store_integration_test.go`

- [ ] **Step 1: Append**

```go
func TestCreateUser_PersistsRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "test@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID.String() == "" || u.Email != "test@example.com" {
		t.Fatalf("user: %+v", u)
	}

	got, err := s.GetUserByEmail(ctx, "test@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("id mismatch: %s vs %s", got.ID, u.ID)
	}
}

func TestGetUserByEmail_ReturnsErrUserNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetUserByEmail(context.Background(), "nobody@example.com")
	if !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("got %v, want ErrUserNotFound", err)
	}
}
```

Run — expect undefined `s.CreateUser`, `s.GetUserByEmail`, `store.ErrUserNotFound`. Capture.

### Task C2: Implement user methods

**Files:**
- Create: `internal/store/users.go`
- Modify: `internal/store/types.go` (add User type)

- [ ] **Step 1: Add User type**

Append to `internal/store/types.go`:

```go
type User struct {
	ID        uuid.UUID
	Email     string
	CreatedAt time.Time
}
```

- [ ] **Step 2: Add user methods**

Create `internal/store/users.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrUserNotFound = errors.New("user not found")

func (s *Store) CreateUser(ctx context.Context, email string) (User, error) {
	id := uuid.New()
	const q = `
		INSERT INTO users (id, email) VALUES ($1, $2)
		RETURNING id, email, created_at`

	var u User
	if err := s.pool.QueryRow(ctx, q, id, email).Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	const q = `SELECT id, email, created_at FROM users WHERE email = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, email).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}
```

Run integration tests — expect 14 pass total in store pkg.

Commit: `feat(store): add CreateUser and GetUserByEmail`

### Task C3: Failing tests for OAuth token methods

- [ ] **Step 1: Append to store_integration_test.go**

```go
func TestSaveOAuthToken_RoundTrip(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "oauth@example.com")

	expires := time.Now().Add(1 * time.Hour).UTC().Truncate(time.Second)
	in := store.OAuthToken{
		UserID:    u.ID,
		Provider:  "google",
		AccessCT:  []byte("encrypted-access"),
		RefreshCT: []byte("encrypted-refresh"),
		ExpiresAt: expires,
	}
	if err := s.SaveOAuthToken(ctx, in); err != nil {
		t.Fatalf("save: %v", err)
	}

	got, err := s.GetOAuthToken(ctx, u.ID, "google")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.UserID != in.UserID || got.Provider != in.Provider {
		t.Fatalf("identity mismatch: %+v", got)
	}
	if string(got.AccessCT) != string(in.AccessCT) || string(got.RefreshCT) != string(in.RefreshCT) {
		t.Fatalf("ciphertext mismatch")
	}
	if !got.ExpiresAt.Equal(expires) {
		t.Fatalf("expires: got %v want %v", got.ExpiresAt, expires)
	}
}

func TestSaveOAuthToken_UpsertsExistingRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "upsert@example.com")

	first := store.OAuthToken{UserID: u.ID, Provider: "google",
		AccessCT: []byte("v1-access"), RefreshCT: []byte("v1-refresh"),
		ExpiresAt: time.Now().Add(time.Hour)}
	_ = s.SaveOAuthToken(ctx, first)

	second := store.OAuthToken{UserID: u.ID, Provider: "google",
		AccessCT: []byte("v2-access"), RefreshCT: []byte("v2-refresh"),
		ExpiresAt: time.Now().Add(2 * time.Hour)}
	if err := s.SaveOAuthToken(ctx, second); err != nil {
		t.Fatalf("upsert: %v", err)
	}

	got, _ := s.GetOAuthToken(ctx, u.ID, "google")
	if string(got.AccessCT) != "v2-access" {
		t.Fatalf("not upserted: %s", got.AccessCT)
	}
}

func TestGetOAuthToken_ReturnsErrOAuthTokenNotFound(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	u, _ := s.CreateUser(ctx, "missing@example.com")

	_, err := s.GetOAuthToken(ctx, u.ID, "google")
	if !errors.Is(err, store.ErrOAuthTokenNotFound) {
		t.Fatalf("got %v, want ErrOAuthTokenNotFound", err)
	}
}
```

Run — expect undefined `store.OAuthToken`, `s.SaveOAuthToken`, `s.GetOAuthToken`, `store.ErrOAuthTokenNotFound`. Capture.

### Task C4: Implement OAuth token methods

**Files:**
- Create: `internal/store/oauth.go`
- Modify: `internal/store/types.go` (add OAuthToken type)

- [ ] **Step 1: Add OAuthToken type**

Append to `internal/store/types.go`:

```go
type OAuthToken struct {
	UserID    uuid.UUID
	Provider  string
	AccessCT  []byte
	RefreshCT []byte
	ExpiresAt time.Time
	UpdatedAt time.Time
}
```

- [ ] **Step 2: Add methods**

Create `internal/store/oauth.go`:

```go
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrOAuthTokenNotFound = errors.New("oauth token not found")

func (s *Store) SaveOAuthToken(ctx context.Context, t OAuthToken) error {
	const q = `
		INSERT INTO oauth_tokens (user_id, provider, access_ct, refresh_ct, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (user_id, provider) DO UPDATE
		SET access_ct = EXCLUDED.access_ct,
		    refresh_ct = EXCLUDED.refresh_ct,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = now()`

	if _, err := s.pool.Exec(ctx, q, t.UserID, t.Provider, t.AccessCT, t.RefreshCT, t.ExpiresAt); err != nil {
		return fmt.Errorf("save oauth: %w", err)
	}
	return nil
}

func (s *Store) GetOAuthToken(ctx context.Context, userID uuid.UUID, provider string) (OAuthToken, error) {
	const q = `
		SELECT user_id, provider, access_ct, refresh_ct, expires_at, updated_at
		FROM oauth_tokens
		WHERE user_id = $1 AND provider = $2`

	var t OAuthToken
	err := s.pool.QueryRow(ctx, q, userID, provider).Scan(
		&t.UserID, &t.Provider, &t.AccessCT, &t.RefreshCT, &t.ExpiresAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthToken{}, ErrOAuthTokenNotFound
	}
	if err != nil {
		return OAuthToken{}, fmt.Errorf("get oauth: %w", err)
	}
	return t, nil
}
```

Run integration tests — expect 17 pass total in store pkg.

Commit: `feat(store): add SaveOAuthToken and GetOAuthToken with ciphertext at rest`

---

## Section D — Local sweep + PR + tag

### Task D1: Sweep

```bash
make lint
make test-unit
make test-integration
make k8s-validate
make loadtest
```

All exit 0.

### Task D2: PR + merge

```bash
git push -u origin phase-3a-oauth-storage

gh pr create --base main --head phase-3a-oauth-storage \
  --title "feat(store,oauth): Phase 3A — User identity + OAuth token storage" \
  --body "$(cat <<'EOF'
## Summary

Phase 3A: foundation slice for the Gmail/Drive integration that ships in 3B–3F.

- Migration 0002: adds users, oauth_tokens, gmail_sync_state, processed_emails. ALTERs pipeline_jobs to add user_id, gmail_message_id, is_synthetic. Partial unique index on (user_id, gmail_message_id) for non-terminal non-synthetic rows.
- internal/oauth: AES-GCM 256 Encrypt/Decrypt helpers. Validates key length (32 bytes). Random nonce per call. Tamper detection.
- internal/store: CreateUser, GetUserByEmail (+ ErrUserNotFound), SaveOAuthToken (UPSERT), GetOAuthToken (+ ErrOAuthTokenNotFound). Tokens stored as opaque ciphertext — encryption happens above the store boundary.

10 new tests (4 oauth unit, 5 store integration, 1 migrate integration). Existing 23 still green.

Phase 3B (Gmail client + History sync) builds directly on top.

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
git tag -a phase-3a -m "Phase 3A: User identity + OAuth token storage"
git push origin phase-3a
gh release create phase-3a --title "Phase 3A: User Identity + OAuth Storage" \
  --notes "Foundation for Gmail/Drive integration. Adds users, oauth_tokens, gmail_sync_state, processed_emails tables; ALTERs pipeline_jobs for multi-tenant columns; ships AES-GCM helpers and store CRUD for users and OAuth tokens. No Google API code yet."
git push origin --delete phase-3a-oauth-storage
git branch -D phase-3a-oauth-storage
```

---

## Out of scope

- Gmail HTTP client or History API polling (Phase 3B)
- Gotenberg / PDF rendering (Phase 3C)
- Drive client (Phase 3D)
- Stage handlers wiring everything together (Phase 3E)
- Scheduler binary (Phase 3F)
- Manual OAuth flow / first real sync (Phase 3F)
