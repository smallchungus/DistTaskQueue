# Phase 2D — Sweeper Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Close the durability loop. Two failure modes get recovered here: a worker that dies mid-job (orphan revival via heartbeat miss) and a job that failed and was scheduled for future retry (delayed-queue promotion when `next_run_at <= now`). Both run on a single sweeper binary every 5 s.

**Architecture:**

```
internal/sweeper/
  sweeper.go                   Sweeper + Run(ctx) + SweepOnce(ctx)
  sweeper_integration_test.go  real PG + Redis, tests both paths
internal/queue/
  (appended) IsWorkerAlive(ctx, workerID) (bool, error)
internal/store/
  (appended) ListRunningJobs(ctx) ([]Job, error)
  (appended) ListReadyRetryJobs(ctx) ([]Job, error)
cmd/sweeper/
  main.go                      wire PG + Redis, signal handling
```

`SweepOnce`:
1. **Orphan revival:** `store.ListRunningJobs()` → for each, `queue.IsWorkerAlive(workerID)` → if false, `store.MarkFailed(jobID, "worker died", now)` (immediate retry — backoff only on handler errors, not worker death).
2. **Delayed promoter:** `store.ListReadyRetryJobs()` returns `status='queued' AND last_error IS NOT NULL AND next_run_at <= now()`. For each, `queue.Push(stage, jobID)`.

`last_error IS NOT NULL` guards against pushing new (un-retried) jobs that a producer is about to push itself. If `last_error` is set, the job has been through the worker at least once and is now waiting on backoff.

Duplicates are safe: if the sweeper pushes an already-in-flight job, the second claim fails with `ErrJobNotClaimable` and the second worker moves on. Worth avoiding but not a correctness issue.

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` Section 7.

**Branch:** `phase-2d-sweeper` → PR to `main`.

---

## Section A — Branch + queue.IsWorkerAlive (TDD)

### Task A1: Branch

- [ ] **Step 1**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-2d-sweeper
```

### Task A2: Failing test for IsWorkerAlive

**Files:**
- Modify: `internal/queue/queue_integration_test.go`

- [ ] **Step 1: Append**

```go
func TestIsWorkerAlive_TrueWhenHeartbeatSet(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	if err := q.Heartbeat(ctx, "w1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	alive, err := q.IsWorkerAlive(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("expected alive=true")
	}
}

func TestIsWorkerAlive_FalseWhenHeartbeatMissing(t *testing.T) {
	q := newQueue(t)
	alive, err := q.IsWorkerAlive(context.Background(), "never-lived")
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("expected alive=false")
	}
}
```

Run `go test -tags=integration ./internal/queue/...` — expect undefined `q.IsWorkerAlive`.

### Task A3: Implement IsWorkerAlive

**Files:**
- Modify: `internal/queue/queue.go`

- [ ] **Step 1: Append**

```go
func (q *Queue) IsWorkerAlive(ctx context.Context, workerID string) (bool, error) {
	n, err := q.cli.Exists(ctx, "heartbeat:"+workerID).Result()
	if err != nil {
		return false, fmt.Errorf("exists: %w", err)
	}
	return n > 0, nil
}
```

Run — expect 7 tests pass in queue pkg (5 existing + 2 new).

Commit: `feat(queue): add IsWorkerAlive reader for heartbeat keys`

---

## Section B — store.ListRunningJobs + ListReadyRetryJobs (TDD)

### Task B1: Failing tests

**Files:**
- Modify: `internal/store/store_integration_test.go`

- [ ] **Step 1: Append**

```go
func TestListRunningJobs_ReturnsOnlyRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	jQueued, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	jRunning, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, jRunning.ID, "w1")
	jDone, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, jDone.ID, "w2")
	_ = s.MarkDone(ctx, jDone.ID)

	jobs, err := s.ListRunningJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].ID != jRunning.ID {
		t.Fatalf("got id %s, want %s", jobs[0].ID, jRunning.ID)
	}
	_ = jQueued
}

func TestListReadyRetryJobs_ReturnsQueuedWithLastErrorAndDueNextRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Ready retry: failed, next_run_at is in the past.
	j1, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j1.ID, "w1")
	past := time.Now().Add(-1 * time.Minute)
	_ = s.MarkFailed(ctx, j1.ID, "boom", past)

	// Not ready: failed, next_run_at in the future.
	j2, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j2.ID, "w1")
	future := time.Now().Add(5 * time.Minute)
	_ = s.MarkFailed(ctx, j2.ID, "boom", future)

	// Newly queued (no last_error) — should not be returned.
	j3, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})

	jobs, err := s.ListReadyRetryJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d, want 1; got IDs: %v", len(jobs), jobIDs(jobs))
	}
	if jobs[0].ID != j1.ID {
		t.Fatalf("got %s, want %s", jobs[0].ID, j1.ID)
	}
	_, _ = j2, j3
}

func jobIDs(js []store.Job) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID.String()
	}
	return out
}
```

Run — expect undefined `s.ListRunningJobs`, `s.ListReadyRetryJobs`.

### Task B2: Implement list methods

**Files:**
- Modify: `internal/store/store.go`

- [ ] **Step 1: Append**

```go
func (s *Store) ListRunningJobs(ctx context.Context) ([]Job, error) {
	const q = `
		SELECT id, stage, status, payload, worker_id, attempts, max_attempts,
		       last_error, next_run_at, claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs WHERE status = $1`
	rows, err := s.pool.Query(ctx, q, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) ListReadyRetryJobs(ctx context.Context) ([]Job, error) {
	const q = `
		SELECT id, stage, status, payload, worker_id, attempts, max_attempts,
		       last_error, next_run_at, claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs
		WHERE status = $1 AND last_error IS NOT NULL AND next_run_at <= now()`
	rows, err := s.pool.Query(ctx, q, StatusQueued)
	if err != nil {
		return nil, fmt.Errorf("list ready retries: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func scanJobs(rows pgx.Rows) ([]Job, error) {
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.Stage, &j.Status, &j.Payload, &j.WorkerID, &j.Attempts,
			&j.MaxAttempts, &j.LastError, &j.NextRunAt, &j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}
```

Run — expect 11 tests pass in store pkg (9 existing + 2 new).

Commit: `feat(store): add ListRunningJobs and ListReadyRetryJobs`

---

## Section C — Sweeper.SweepOnce (TDD integration)

### Task C1: Failing tests for both sweep paths

**Files:**
- Create: `internal/sweeper/sweeper_integration_test.go`

- [ ] **Step 1**

```go
//go:build integration

package sweeper_test

import (
	"context"
	"testing"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/sweeper"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func setup(t *testing.T) (*store.Store, *queue.Queue, *sweeper.Sweeper) {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))
	sw := sweeper.New(sweeper.Config{Store: s, Queue: q})
	return s, q, sw
}

func TestSweepOnce_RequeuesJobWithDeadWorker(t *testing.T) {
	s, q, sw := setup(t)
	ctx := context.Background()

	// Heartbeat briefly, then simulate death by not renewing.
	if err := q.Heartbeat(ctx, "w1", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	job, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	if err := s.ClaimJob(ctx, job.ID, "w1"); err != nil {
		t.Fatal(err)
	}
	// Let heartbeat TTL expire.
	time.Sleep(700 * time.Millisecond)

	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	got, _ := s.GetJob(ctx, job.ID)
	if got.Status != store.StatusQueued {
		t.Fatalf("status: %s, want queued (revived)", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts: %d, want 1", got.Attempts)
	}
	if got.LastError == nil || *got.LastError != "worker died" {
		t.Fatalf("last_error: %v, want 'worker died'", got.LastError)
	}
}

func TestSweepOnce_DoesNotRequeueJobWithLiveWorker(t *testing.T) {
	s, q, sw := setup(t)
	ctx := context.Background()

	if err := q.Heartbeat(ctx, "w1", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	job, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, job.ID, "w1")

	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	got, _ := s.GetJob(ctx, job.ID)
	if got.Status != store.StatusRunning {
		t.Fatalf("status: %s, want running (untouched)", got.Status)
	}
}

func TestSweepOnce_PromotesDelayedRetryToRedisQueue(t *testing.T) {
	s, q, sw := setup(t)
	ctx := context.Background()

	job, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, job.ID, "w1")
	_ = s.MarkFailed(ctx, job.ID, "boom", time.Now().Add(-1*time.Second))

	depthBefore, _ := q.Depth(ctx, "test")
	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	depthAfter, _ := q.Depth(ctx, "test")

	if depthAfter != depthBefore+1 {
		t.Fatalf("queue depth: before=%d after=%d, want +1", depthBefore, depthAfter)
	}

	popped, err := q.BlockingPop(ctx, "test", 1*time.Second)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if popped != job.ID.String() {
		t.Fatalf("popped %q, want %q", popped, job.ID.String())
	}
}

func TestSweepOnce_LeavesFutureRetryInPostgresOnly(t *testing.T) {
	s, q, sw := setup(t)
	ctx := context.Background()

	job, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, job.ID, "w1")
	_ = s.MarkFailed(ctx, job.ID, "boom", time.Now().Add(1*time.Hour))

	depthBefore, _ := q.Depth(ctx, "test")
	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	depthAfter, _ := q.Depth(ctx, "test")

	if depthAfter != depthBefore {
		t.Fatalf("queue depth changed: before=%d after=%d, want unchanged (future retry)", depthBefore, depthAfter)
	}
}
```

Run `go test -tags=integration ./internal/sweeper/...` — expect undefined `sweeper.New`, `sweeper.Config`, `sweeper.Sweeper.SweepOnce`.

### Task C2: Implement Sweeper + SweepOnce

**Files:**
- Create: `internal/sweeper/sweeper.go`

- [ ] **Step 1**

```go
package sweeper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Store    *store.Store
	Queue    *queue.Queue
	Interval time.Duration
}

type Sweeper struct {
	cfg Config
}

func New(cfg Config) *Sweeper {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	return &Sweeper{cfg: cfg}
}

func (s *Sweeper) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := s.SweepOnce(ctx); err != nil {
				slog.Error("sweep failed", "err", err)
			}
		}
	}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
	if err := s.reviveOrphans(ctx); err != nil {
		return fmt.Errorf("revive orphans: %w", err)
	}
	if err := s.promoteDelayed(ctx); err != nil {
		return fmt.Errorf("promote delayed: %w", err)
	}
	return nil
}

func (s *Sweeper) reviveOrphans(ctx context.Context) error {
	running, err := s.cfg.Store.ListRunningJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range running {
		if j.WorkerID == nil {
			continue
		}
		alive, err := s.cfg.Queue.IsWorkerAlive(ctx, *j.WorkerID)
		if err != nil {
			slog.Warn("heartbeat check failed", "job_id", j.ID, "err", err)
			continue
		}
		if alive {
			continue
		}
		if err := s.cfg.Store.MarkFailed(ctx, j.ID, "worker died", time.Now()); err != nil {
			slog.Warn("revive failed", "job_id", j.ID, "err", err)
			continue
		}
		slog.Info("revived orphan", "job_id", j.ID, "worker_id", *j.WorkerID)
	}
	return nil
}

func (s *Sweeper) promoteDelayed(ctx context.Context) error {
	ready, err := s.cfg.Store.ListReadyRetryJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range ready {
		if err := s.cfg.Queue.Push(ctx, j.Stage, j.ID.String()); err != nil {
			slog.Warn("promote failed", "job_id", j.ID, "err", err)
			continue
		}
		slog.Info("promoted retry", "job_id", j.ID, "stage", j.Stage)
	}
	return nil
}
```

Run — expect 4 tests pass.

Commit: `feat(sweeper): add Sweeper with orphan revival and delayed-retry promotion`

---

## Section D — cmd/sweeper main

### Task D1: Wire main

**Files:**
- Create: `cmd/sweeper/main.go`

- [ ] **Step 1**

```go
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/sweeper"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dsn := envOr("DATABASE_URL", "postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")

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

	sw := sweeper.New(sweeper.Config{
		Store:    store.New(pool),
		Queue:    queue.New(redis),
		Interval: 5 * time.Second,
	})

	slog.Info("sweeper starting")
	if err := sw.Run(ctx); err != nil {
		slog.Error("sweeper run", "err", err)
		os.Exit(1)
	}
	slog.Info("sweeper stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Build and smoke test**

```bash
go build ./cmd/sweeper
docker compose up -d postgres redis
sleep 5
DATABASE_URL='postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable' \
REDIS_URL='redis://localhost:6379/0' \
./sweeper &
SWP=$!
sleep 2
kill -TERM $SWP
wait $SWP 2>/dev/null || true
docker compose down -v
```

Expected log lines: `sweeper starting` then `sweeper stopped`.

Commit: `feat(sweeper): add cmd/sweeper main with signal handling`

---

## Section E — Local sweep + PR + tag

### Task E1: Sweep

```bash
make lint
make test-unit
make test-integration
make k8s-validate
```

All exit 0.

### Task E2: PR + merge

```bash
git push -u origin phase-2d-sweeper
gh pr create --base main --head phase-2d-sweeper \
  --title "feat(sweeper): Phase 2D — Orphan revival and delayed-retry promotion" \
  --body "$(cat <<'EOF'
## Summary

Phase 2D: the durability loop.

- `internal/sweeper` — `SweepOnce(ctx)` does two passes:
  1. Orphan revival: finds `status=running` jobs whose `worker_id` has no live heartbeat, calls `MarkFailed` with "worker died". Status goes back to queued (or dead if max attempts hit).
  2. Delayed-retry promotion: finds `status=queued AND last_error IS NOT NULL AND next_run_at <= now()` jobs and pushes their IDs back onto the Redis stage queue.
- New queue method: `IsWorkerAlive(workerID)` — `EXISTS heartbeat:<id>`.
- New store methods: `ListRunningJobs`, `ListReadyRetryJobs`, plus shared `scanJobs` helper.
- `cmd/sweeper` binary with 5 s tick + signal handling.

4 sweeper integration tests (orphan revival happy + skip, delayed promotion happy + skip future). Existing tests still green.

## Test plan

- [x] make lint
- [x] make test-unit
- [x] make test-integration (sweeper, worker, queue, store, testutil, api — all green)
- [x] make k8s-validate
- [x] cmd/sweeper smoke run
- [ ] CI green
EOF
)"

PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-2d -m "Phase 2D: Sweeper — orphan revival + delayed retry promotion"
git push origin phase-2d
gh release create phase-2d --title "Phase 2D: Sweeper" \
  --notes "Orphan revival (heartbeat-miss detection) and delayed-retry promotion back onto the Redis queue. Closes the durability loop: a worker that dies mid-job is safely requeued; a job that failed and backed off gets picked back up when its next_run_at arrives."
git push origin --delete phase-2d-sweeper
git branch -D phase-2d-sweeper
```

---

## Out of scope

- 5K-in-10s load test (Phase 2E)
- Gmail / Drive / PDF handlers (Phase 3)
- HPA-driving Prometheus metrics (Phase 1.5)
- Dashboard (Phase 4)
