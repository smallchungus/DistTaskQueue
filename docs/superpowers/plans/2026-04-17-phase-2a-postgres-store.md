# Phase 2A — Postgres Store Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the durable state layer for the queue. Postgres schema (pipeline_jobs + job_status_history), migrations infrastructure, and a `Store` type with the CRUD methods the queue needs: enqueue, get, claim, mark-done, mark-failed. No Redis, no workers, no Gmail — pure Postgres CRUD with strict TDD.

**Architecture:** `internal/store` package wraps a `*pgxpool.Pool`. SQL migrations under `internal/store/migrations/` are embedded into the binary via `embed.FS` and applied via `golang-migrate`. Every method gets an integration test against a real Postgres container (testcontainers-go from Phase 1). No mocks of `pgx`. Each method is one TDD cycle: write failing integration test, see it fail, implement, see it pass, commit.

**Tech Stack:** `pgx/v5`, `golang-migrate/v4` with `iofs` source + `pgx/v5` database driver, `google/uuid`, existing `testcontainers-go` from Phase 1.

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` Sections 5.1 and 7. Phase 2A intentionally trims the spec's Phase 3 columns (`user_id`, `gmail_message_id`) — those land via ALTER TABLE in Phase 3 when Gmail integration starts.

**Branch strategy:** create a feature branch `phase-2a-postgres-store` and PR back to `main` when done. Use GitHub's auto-merge once CI is green.

---

## Section A — Dependencies + branch

### Task A1: Branch and dependencies

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Create feature branch**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout -b phase-2a-postgres-store
```

- [ ] **Step 2: Add modules**

```bash
go get github.com/golang-migrate/migrate/v4
go get github.com/golang-migrate/migrate/v4/source/iofs
go get github.com/golang-migrate/migrate/v4/database/pgx/v5
go get github.com/google/uuid
go mod tidy
```

- [ ] **Step 3: Verify build**

```bash
go build ./...
```

Expected: silent success.

- [ ] **Step 4: Commit**

```bash
git add go.mod go.sum
git commit -m "chore(store): add golang-migrate, uuid deps"
```

---

## Section B — Migration files + Migrate() function

### Task B1: Initial migration up + down

**Files:**
- Create: `internal/store/migrations/0001_init.up.sql`
- Create: `internal/store/migrations/0001_init.down.sql`

- [ ] **Step 1: Write up migration**

Create `internal/store/migrations/0001_init.up.sql`:

```sql
CREATE TABLE pipeline_jobs (
  id              UUID PRIMARY KEY,
  stage           TEXT NOT NULL,
  status          TEXT NOT NULL,
  payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
  worker_id       TEXT,
  attempts        INT NOT NULL DEFAULT 0,
  max_attempts    INT NOT NULL DEFAULT 8,
  last_error      TEXT,
  next_run_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
  claimed_at      TIMESTAMPTZ,
  completed_at    TIMESTAMPTZ,
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX pipeline_jobs_stage_status_idx ON pipeline_jobs (stage, status);
CREATE INDEX pipeline_jobs_queued_next_run_idx ON pipeline_jobs (status, next_run_at) WHERE status = 'queued';

CREATE TABLE job_status_history (
  id            BIGSERIAL PRIMARY KEY,
  job_id        UUID NOT NULL REFERENCES pipeline_jobs(id) ON DELETE CASCADE,
  stage         TEXT NOT NULL,
  from_status   TEXT,
  to_status     TEXT NOT NULL,
  worker_id     TEXT,
  error         TEXT,
  at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX job_status_history_job_at_idx ON job_status_history (job_id, at);
```

- [ ] **Step 2: Write down migration**

Create `internal/store/migrations/0001_init.down.sql`:

```sql
DROP TABLE IF EXISTS job_status_history;
DROP TABLE IF EXISTS pipeline_jobs;
```

### Task B2: Failing test for Migrate()

**Files:**
- Test: `internal/store/migrate_integration_test.go`

- [ ] **Step 1: Write the failing integration test**

Create `internal/store/migrate_integration_test.go`:

```go
//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func TestMigrate_AppliesInitialSchema(t *testing.T) {
	pool := testutil.StartPostgres(t)

	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('pipeline_jobs','job_status_history')`,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 tables, got %d", n)
	}
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test -tags=integration ./internal/store/...
```

Expected build error: package `store` doesn't exist yet.

### Task B3: Implement Migrate()

**Files:**
- Create: `internal/store/migrate.go`

- [ ] **Step 1: Write Migrate function**

Create `internal/store/migrate.go`:

```go
package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func Migrate(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()

	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{})
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}

	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx", driver)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("up: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/store/...
```

Expected: `ok github.com/smallchungus/disttaskqueue/internal/store`. Capture output.

- [ ] **Step 3: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add initial migration and Migrate() entrypoint"
```

---

## Section C — Job model + EnqueueJob (TDD)

### Task C1: Define types and EnqueueJob test

**Files:**
- Test: `internal/store/store_integration_test.go`
- Create: `internal/store/types.go` (defined in C2)

- [ ] **Step 1: Write the failing test**

Create `internal/store/store_integration_test.go`:

```go
//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.New(pool)
}

func TestEnqueueJob_PersistsRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	j, err := s.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"k":"v"}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if j.ID.String() == "" || j.Stage != "test" || j.Status != store.StatusQueued {
		t.Fatalf("unexpected job: %+v", j)
	}
	if string(j.Payload) != `{"k": "v"}` && string(j.Payload) != `{"k":"v"}` {
		t.Fatalf("payload roundtrip: got %s", string(j.Payload))
	}
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test -tags=integration ./internal/store/...
```

Expected: undefined `store.Store`, `store.New`, `store.NewJob`, `store.StatusQueued`, `store.EnqueueJob`.

### Task C2: Implement types and EnqueueJob

**Files:**
- Create: `internal/store/types.go`
- Create: `internal/store/store.go`

- [ ] **Step 1: Write types**

Create `internal/store/types.go`:

```go
package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type JobStatus string

const (
	StatusQueued  JobStatus = "queued"
	StatusRunning JobStatus = "running"
	StatusDone    JobStatus = "done"
	StatusDead    JobStatus = "dead"
)

type Job struct {
	ID          uuid.UUID
	Stage       string
	Status      JobStatus
	Payload     json.RawMessage
	WorkerID    *string
	Attempts    int
	MaxAttempts int
	LastError   *string
	NextRunAt   time.Time
	ClaimedAt   *time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type NewJob struct {
	Stage   string
	Payload json.RawMessage
}
```

- [ ] **Step 2: Write store.go with EnqueueJob**

Create `internal/store/store.go`:

```go
package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) EnqueueJob(ctx context.Context, nj NewJob) (Job, error) {
	id := uuid.New()
	payload := nj.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}

	const q = `
		INSERT INTO pipeline_jobs (id, stage, status, payload)
		VALUES ($1, $2, $3, $4)
		RETURNING id, stage, status, payload, worker_id, attempts, max_attempts,
		          last_error, next_run_at, claimed_at, completed_at, created_at, updated_at`

	row := s.pool.QueryRow(ctx, q, id, nj.Stage, StatusQueued, payload)
	var j Job
	if err := row.Scan(&j.ID, &j.Stage, &j.Status, &j.Payload, &j.WorkerID, &j.Attempts,
		&j.MaxAttempts, &j.LastError, &j.NextRunAt, &j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return Job{}, fmt.Errorf("enqueue: %w", err)
	}
	return j, nil
}
```

- [ ] **Step 3: Run, expect pass**

```bash
go test -tags=integration ./internal/store/...
```

Expected: 2 tests pass (Migrate + EnqueueJob).

- [ ] **Step 4: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add Store with EnqueueJob"
```

---

## Section D — GetJob (TDD)

### Task D1: Failing test for GetJob

**Files:**
- Modify: `internal/store/store_integration_test.go`

- [ ] **Step 1: Append the test**

```go
func TestGetJob_RoundTripsAllFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	enq, err := s.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetJob(ctx, enq.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != enq.ID || got.Stage != enq.Stage || got.Status != enq.Status {
		t.Fatalf("mismatch: enq=%+v got=%+v", enq, got)
	}
}

func TestGetJob_ReturnsErrNotFoundForUnknownID(t *testing.T) {
	s := newStore(t)
	_, err := s.GetJob(context.Background(), uuid.New())
	if !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("got %v, want ErrJobNotFound", err)
	}
}
```

You'll need to add imports: `errors`, `github.com/google/uuid`.

- [ ] **Step 2: Run, expect failure**

```bash
go test -tags=integration ./internal/store/...
```

Expected: undefined `store.GetJob`, `store.ErrJobNotFound`.

### Task D2: Implement GetJob

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Add ErrJobNotFound and GetJob**

Append to `internal/store/store.go`:

```go
import (
	"errors"
	// ... existing imports plus errors
	"github.com/jackc/pgx/v5"
)

var ErrJobNotFound = errors.New("job not found")

func (s *Store) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	const q = `
		SELECT id, stage, status, payload, worker_id, attempts, max_attempts,
		       last_error, next_run_at, claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs WHERE id = $1`

	row := s.pool.QueryRow(ctx, q, id)
	var j Job
	err := row.Scan(&j.ID, &j.Stage, &j.Status, &j.Payload, &j.WorkerID, &j.Attempts,
		&j.MaxAttempts, &j.LastError, &j.NextRunAt, &j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get: %w", err)
	}
	return j, nil
}
```

Consolidate the import block at the top of the file.

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/store/...
```

Expected: 4 tests pass.

- [ ] **Step 3: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add GetJob and ErrJobNotFound"
```

---

## Section E — ClaimJob (TDD, atomic)

### Task E1: Failing test for ClaimJob (happy path + concurrent loser)

**Files:**
- Modify: `internal/store/store_integration_test.go`

- [ ] **Step 1: Append the tests**

```go
func TestClaimJob_TransitionsQueuedToRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})

	if err := s.ClaimJob(ctx, j.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusRunning {
		t.Fatalf("status: %s", got.Status)
	}
	if got.WorkerID == nil || *got.WorkerID != "worker-1" {
		t.Fatalf("worker_id: %v", got.WorkerID)
	}
	if got.ClaimedAt == nil {
		t.Fatalf("claimed_at: nil")
	}
}

func TestClaimJob_FailsWhenNotQueued(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	if err := s.ClaimJob(ctx, j.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	err := s.ClaimJob(ctx, j.ID, "worker-2")
	if !errors.Is(err, store.ErrJobNotClaimable) {
		t.Fatalf("got %v, want ErrJobNotClaimable", err)
	}
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test -tags=integration ./internal/store/...
```

Expected: undefined `store.ClaimJob`, `store.ErrJobNotClaimable`.

### Task E2: Implement ClaimJob

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Add ClaimJob**

Append:

```go
var ErrJobNotClaimable = errors.New("job not claimable")

func (s *Store) ClaimJob(ctx context.Context, id uuid.UUID, workerID string) error {
	const q = `
		UPDATE pipeline_jobs
		SET status = $1, worker_id = $2, claimed_at = now(), updated_at = now()
		WHERE id = $3 AND status = $4
		RETURNING id`

	var got uuid.UUID
	err := s.pool.QueryRow(ctx, q, StatusRunning, workerID, id, StatusQueued).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrJobNotClaimable
	}
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	return nil
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/store/...
```

Expected: 6 tests pass.

- [ ] **Step 3: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add ClaimJob with atomic queued->running transition"
```

---

## Section F — MarkDone (TDD)

### Task F1: Failing test

**Files:**
- Modify: `internal/store/store_integration_test.go`

- [ ] **Step 1: Append**

```go
func TestMarkDone_SetsTerminalState(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j.ID, "worker-1")

	if err := s.MarkDone(ctx, j.ID); err != nil {
		t.Fatalf("done: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusDone {
		t.Fatalf("status: %s", got.Status)
	}
	if got.CompletedAt == nil {
		t.Fatalf("completed_at: nil")
	}
}
```

- [ ] **Step 2: Run, expect failure**

Expected: undefined `store.MarkDone`.

### Task F2: Implement MarkDone

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Add MarkDone**

```go
func (s *Store) MarkDone(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE pipeline_jobs
		SET status = $1, completed_at = now(), updated_at = now()
		WHERE id = $2`

	tag, err := s.pool.Exec(ctx, q, StatusDone, id)
	if err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}
```

- [ ] **Step 2: Run, expect pass**

Expected: 7 tests pass.

- [ ] **Step 3: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add MarkDone for terminal success state"
```

---

## Section G — MarkFailed (TDD, with retry/dead branching)

### Task G1: Failing tests for retry path AND dead path

**Files:**
- Modify: `internal/store/store_integration_test.go`

- [ ] **Step 1: Append**

```go
func TestMarkFailed_RequeuesWithIncrementedAttempts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j.ID, "worker-1")

	nextRun := time.Now().Add(2 * time.Second)
	if err := s.MarkFailed(ctx, j.ID, "transient boom", nextRun); err != nil {
		t.Fatalf("fail: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusQueued {
		t.Fatalf("status: %s, want queued", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts: %d, want 1", got.Attempts)
	}
	if got.LastError == nil || *got.LastError != "transient boom" {
		t.Fatalf("last_error: %v", got.LastError)
	}
	if got.WorkerID != nil {
		t.Fatalf("worker_id: %v, want nil after fail", got.WorkerID)
	}
}

func TestMarkFailed_MarksDeadAtMaxAttempts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})

	for i := 0; i < 8; i++ {
		_ = s.ClaimJob(ctx, j.ID, "worker-1")
		_ = s.MarkFailed(ctx, j.ID, "still broken", time.Now())
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusDead {
		t.Fatalf("status: %s, want dead", got.Status)
	}
}
```

You'll need to import `time`.

- [ ] **Step 2: Run, expect failure**

Expected: undefined `store.MarkFailed`.

### Task G2: Implement MarkFailed

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Add MarkFailed**

```go
func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, errMsg string, nextRunAt time.Time) error {
	const q = `
		UPDATE pipeline_jobs
		SET attempts = attempts + 1,
		    status = CASE WHEN attempts + 1 >= max_attempts THEN $1::text ELSE $2::text END,
		    worker_id = NULL,
		    last_error = $3,
		    next_run_at = $4,
		    updated_at = now()
		WHERE id = $5`

	tag, err := s.pool.Exec(ctx, q, StatusDead, StatusQueued, errMsg, nextRunAt, id)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}
```

You'll need to add `"time"` to imports.

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/store/...
```

Expected: 9 tests pass.

- [ ] **Step 3: Commit**

```bash
git add internal/store/
git commit -m "feat(store): add MarkFailed with retry/dead branching"
```

---

## Section H — Final verification + PR

### Task H1: Full local sweep

- [ ] **Step 1: Run all checks**

```bash
cd /Users/willchen/Development/DistTaskQueue
make lint
make test-unit
make test-integration
make k8s-validate
```

Expected: every command exits 0.

- [ ] **Step 2: Push branch and open PR**

```bash
git push -u origin phase-2a-postgres-store
gh pr create --title "feat(store): Phase 2A — Postgres store with migrations and CRUD" --body "$(cat <<'EOF'
## Summary

Phase 2A of the queue: durable state layer.

- Initial migration creates `pipeline_jobs` and `job_status_history` tables
- `Store` type wrapping `*pgxpool.Pool` with the queue's CRUD primitives:
  - `EnqueueJob`, `GetJob`, `ClaimJob` (atomic), `MarkDone`, `MarkFailed` (with retry/dead branching)
- `Migrate(ctx, dsn)` applies embedded SQL migrations via golang-migrate + iofs
- 9 integration tests against real Postgres via testcontainers-go (no mocks)

Phase 3 will ALTER TABLE to add Gmail-specific columns (`user_id`, `gmail_message_id`).

## Test plan

- [x] `make lint` clean
- [x] `make test-unit` passing
- [x] `make test-integration` passing (9 store tests + 2 testutil smoke tests)
- [x] `make k8s-validate` passing
- [ ] CI green (will verify after push)
EOF
)"
```

- [ ] **Step 3: Wait for CI, enable auto-merge once green**

```bash
sleep 5
gh pr view --json number -q .number
# Wait for CI:
PR_NUMBER=$(gh pr view --json number -q .number)
gh pr checks --watch $PR_NUMBER
gh pr merge --auto --squash $PR_NUMBER
```

The `--auto` flag enables GitHub auto-merge; the PR squash-merges as soon as required checks pass.

- [ ] **Step 4: Confirm merge and tag**

```bash
git checkout main
git pull
git tag -a phase-2a -m "Phase 2A: Postgres store with migrations and CRUD"
git push origin phase-2a
gh release create phase-2a --title "Phase 2A: Postgres Store" --notes "Durable state layer for the queue. See PR for details."
```

---

## Out of scope for Phase 2A (no scope creep)

These are explicit non-goals in 2A; do NOT implement:

- Redis queue (Phase 2B)
- Worker binary / heartbeats (Phase 2C)
- Sweeper (Phase 2D)
- End-to-end synthetic workload (Phase 2E)
- Gmail / Drive columns (Phase 3)
- Dashboard schema queries (Phase 4)

## Phase 2B preview (separate plan)

Redis-backed `Queue` type: `Push(ctx, stage, jobID)`, `BlockingPop(ctx, stage) (jobID, error)`, `Depth(ctx, stage) (int64, error)`. Integration tests against real Redis. Sweeper-related delayed-queue (ZSET) deferred to 2D.
