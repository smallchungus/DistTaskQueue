# Phase 2C — Worker Scaffold Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Build the unit that pulls a job ID off a Redis queue, claims it in Postgres, runs a stage handler, writes heartbeats, and reports success/failure back to Postgres. The worker is one Go binary (`cmd/worker --stage=<name>`) that ties the Phase 2A `store` and Phase 2B `queue` packages together. One sample handler (`NoopHandler`) ships in this phase so we can drive the pipeline end-to-end in 2E. Real Gmail/Drive handlers land in Phase 3.

**Architecture:**

```
internal/worker/
  backoff.go         backoff.Compute(attempts) time.Duration  [pure, unit-tested]
  id.go              NewWorkerID() string                     [pure, unit-tested]
  handler.go         Handler interface + NoopHandler          [pure, unit-tested]
  worker.go          Worker struct, New(), Run(), ProcessOne()
  worker_integration_test.go        end-to-end: enqueue -> ProcessOne -> store check
  heartbeat_integration_test.go     heartbeat key set under TTL during processing
internal/queue/
  (queue.go appended)  Heartbeat(ctx, workerID, ttl) error + Client() accessor
cmd/worker/
  main.go            flag parsing, wiring, slog, signal handling
```

`ProcessOne(ctx)` is the testable atom: pop one job, claim, heartbeat, run handler, mark done/failed. Returns `(didWork bool, err error)`. `Run(ctx)` loops calling `ProcessOne` until `ctx.Done()`. Tests drive `ProcessOne`; we trust the loop.

Heartbeat is a goroutine spawned at handler start, cancelled when handler returns. Calls `queue.Heartbeat(ctx, workerID, 15s)` every 5 s. TTL > 2× interval so a single missed write doesn't expire.

`workerID` = `<hostname>-<8 hex chars random>` so two pods on the same node don't collide. Generated once at startup.

`NoopHandler.Process(ctx, job)` reads `payload.sleepMs` (default 0), sleeps that long, returns nil. Used in 2C tests and the 2E synthetic workload.

**Tech Stack:** existing deps. Adds nothing new.

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` Sections 4 and 7.

**Branch:** `phase-2c-worker` → PR to `main`.

---

## Section A — Branch + backoff (pure unit TDD)

### Task A1: Branch off main

- [ ] **Step 1**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-2c-worker
```

### Task A2: Failing test for backoff.Compute

**Files:**
- Create: `internal/worker/backoff_test.go`

- [ ] **Step 1: Write tests**

```go
package worker

import (
	"testing"
	"time"
)

func TestCompute_GrowsExponentially(t *testing.T) {
	cases := []struct {
		attempts int
		minD     time.Duration
		maxD     time.Duration
	}{
		{1, 1500 * time.Millisecond, 2500 * time.Millisecond},
		{2, 3 * time.Second, 5 * time.Second},
		{3, 6 * time.Second, 10 * time.Second},
		{4, 12 * time.Second, 20 * time.Second},
	}
	for _, c := range cases {
		t.Run("", func(t *testing.T) {
			d := Compute(c.attempts)
			if d < c.minD || d > c.maxD {
				t.Fatalf("attempts=%d got %v, want in [%v,%v]", c.attempts, d, c.minD, c.maxD)
			}
		})
	}
}

func TestCompute_CapsAt600s(t *testing.T) {
	d := Compute(20)
	if d < 450*time.Second || d > 750*time.Second {
		t.Fatalf("got %v, want ~600s ±25%%", d)
	}
}
```

Run `go test ./internal/worker/...` — expect undefined `Compute`. Capture.

### Task A3: Implement backoff

**Files:**
- Create: `internal/worker/backoff.go`

- [ ] **Step 1: Write Compute**

```go
package worker

import (
	"math"
	"math/rand"
	"time"
)

func Compute(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	base := math.Pow(2, float64(attempts))
	if base > 600 {
		base = 600
	}
	jitter := 0.75 + rand.Float64()*0.5
	return time.Duration(base * jitter * float64(time.Second))
}
```

Run tests — expect 5 subtests pass. Capture.

Commit: `feat(worker): add backoff.Compute with exponential growth and ±25% jitter`

---

## Section B — workerID + Handler + NoopHandler (pure)

### Task B1: workerID test

**Files:**
- Create: `internal/worker/id_test.go`

- [ ] **Step 1**

```go
package worker

import (
	"strings"
	"testing"
)

func TestNewWorkerID_HasHostnamePrefixAndRandomSuffix(t *testing.T) {
	id := NewWorkerID()
	if !strings.Contains(id, "-") {
		t.Fatalf("missing hyphen: %q", id)
	}
	parts := strings.SplitN(id, "-", 2)
	if len(parts[0]) == 0 {
		t.Fatalf("empty hostname: %q", id)
	}
	if len(parts[1]) != 16 {
		t.Fatalf("suffix not 16 hex chars: %q", id)
	}
}

func TestNewWorkerID_IsUniqueAcrossCalls(t *testing.T) {
	a := NewWorkerID()
	b := NewWorkerID()
	if a == b {
		t.Fatalf("ids collided: %q", a)
	}
}
```

Run — expect undefined `NewWorkerID`.

### Task B2: Implement NewWorkerID

**Files:**
- Create: `internal/worker/id.go`

- [ ] **Step 1**

```go
package worker

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
)

func NewWorkerID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s-%s", host, hex.EncodeToString(b[:]))
}
```

Run — expect 2 tests pass.

Commit: `feat(worker): add NewWorkerID (hostname + 8 random bytes)`

### Task B3: Handler + NoopHandler test

**Files:**
- Create: `internal/worker/handler_test.go`

- [ ] **Step 1**

```go
package worker

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

func TestNoopHandler_SleepsThenReturnsNil(t *testing.T) {
	h := NoopHandler{}
	job := store.Job{ID: uuid.New(), Payload: json.RawMessage(`{"sleepMs":50}`)}

	start := time.Now()
	err := h.Process(context.Background(), job)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if elapsed < 50*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("elapsed %v, want ~50ms", elapsed)
	}
}

func TestNoopHandler_DefaultsToZeroSleep(t *testing.T) {
	h := NoopHandler{}
	job := store.Job{ID: uuid.New(), Payload: json.RawMessage(`{}`)}

	start := time.Now()
	if err := h.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("should be instant")
	}
}
```

Run — expect undefined `Handler`, `NoopHandler`.

### Task B4: Implement Handler + NoopHandler

**Files:**
- Create: `internal/worker/handler.go`

- [ ] **Step 1**

```go
package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Handler interface {
	Process(ctx context.Context, job store.Job) error
}

type NoopHandler struct{}

func (NoopHandler) Process(ctx context.Context, job store.Job) error {
	var p struct {
		SleepMs int `json:"sleepMs"`
	}
	if len(job.Payload) > 0 {
		_ = json.Unmarshal(job.Payload, &p)
	}
	if p.SleepMs <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(time.Duration(p.SleepMs) * time.Millisecond):
		return nil
	}
}
```

Run — expect 2 tests pass.

Commit: `feat(worker): add Handler interface and NoopHandler`

---

## Section C — Heartbeat on Queue (TDD)

Heartbeat lives in the `queue` package (shares the Redis client). Sweeper (Phase 2D) will add the reader.

### Task C1: Failing test

**Files:**
- Modify: `internal/queue/queue_integration_test.go`

- [ ] **Step 1: Append**

```go
func TestHeartbeat_SetsKeyWithTTL(t *testing.T) {
	q := newQueue(t)
	cli := q.Client()

	if err := q.Heartbeat(context.Background(), "worker-xyz", 5*time.Second); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	val, err := cli.Get(context.Background(), "heartbeat:worker-xyz").Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val == "" {
		t.Fatalf("empty value")
	}
	ttl, err := cli.TTL(context.Background(), "heartbeat:worker-xyz").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > 5*time.Second {
		t.Fatalf("ttl out of range: %v", ttl)
	}
}
```

Run — expect undefined `q.Heartbeat`, `q.Client`.

### Task C2: Implement Heartbeat + Client accessor

**Files:**
- Modify: `internal/queue/queue.go`

- [ ] **Step 1: Append (and add `"strconv"` to imports)**

```go
// Client exposes the underlying Redis client. Test-only.
func (q *Queue) Client() *goredis.Client { return q.cli }

func (q *Queue) Heartbeat(ctx context.Context, workerID string, ttl time.Duration) error {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if err := q.cli.Set(ctx, "heartbeat:"+workerID, now, ttl).Err(); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}
```

Run — expect 5 tests in queue pkg.

Commit: `feat(queue): add Heartbeat writer and Client accessor`

---

## Section D — Worker.ProcessOne (TDD, the heart)

### Task D1: Failing integration tests

**Files:**
- Create: `internal/worker/worker_integration_test.go`

- [ ] **Step 1**

```go
//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
	"github.com/smallchungus/disttaskqueue/internal/worker"
)

type harness struct {
	store *store.Store
	queue *queue.Queue
	w     *worker.Worker
}

func setupHarness(t *testing.T, h worker.Handler) *harness {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))
	w := worker.New(worker.Config{
		Stage:             "test",
		WorkerID:          "worker-test-1",
		Store:             s,
		Queue:             q,
		Handler:           h,
		PopTimeout:        500 * time.Millisecond,
		HeartbeatTTL:      5 * time.Second,
		HeartbeatInterval: 1 * time.Second,
	})
	return &harness{store: s, queue: q, w: w}
}

func TestProcessOne_RunsHandlerAndMarksDone(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx := context.Background()

	job, err := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"sleepMs":20}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.queue.Push(ctx, "test", job.ID.String()); err != nil {
		t.Fatal(err)
	}

	didWork, err := h.w.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if !didWork {
		t.Fatal("expected didWork=true")
	}

	got, _ := h.store.GetJob(ctx, job.ID)
	if got.Status != store.StatusDone {
		t.Fatalf("status: %s, want done", got.Status)
	}
}

func TestProcessOne_ReturnsNoWorkOnEmptyQueue(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	didWork, err := h.w.ProcessOne(context.Background())
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if didWork {
		t.Fatal("expected didWork=false")
	}
}

type errHandler struct{}

func (errHandler) Process(ctx context.Context, job store.Job) error {
	return &handlerErr{"boom"}
}

type handlerErr struct{ msg string }

func (e *handlerErr) Error() string { return e.msg }

func TestProcessOne_MarksFailedOnHandlerError(t *testing.T) {
	h := setupHarness(t, errHandler{})
	ctx := context.Background()

	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = h.queue.Push(ctx, "test", job.ID.String())

	didWork, err := h.w.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if !didWork {
		t.Fatal("expected didWork=true")
	}

	got, _ := h.store.GetJob(ctx, job.ID)
	if got.Status != store.StatusQueued {
		t.Fatalf("status: %s, want queued (retry)", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts: %d, want 1", got.Attempts)
	}
	if got.LastError == nil || *got.LastError != "boom" {
		t.Fatalf("last_error: %v", got.LastError)
	}
}
```

Run — expect undefined `worker.New`, `worker.Config`, `*Worker.ProcessOne`.

### Task D2: Implement Worker

**Files:**
- Create: `internal/worker/worker.go`

- [ ] **Step 1**

```go
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Stage             string
	WorkerID          string
	Store             *store.Store
	Queue             *queue.Queue
	Handler           Handler
	PopTimeout        time.Duration
	HeartbeatTTL      time.Duration
	HeartbeatInterval time.Duration
}

type Worker struct {
	cfg Config
}

func New(cfg Config) *Worker {
	return &Worker{cfg: cfg}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if _, err := w.ProcessOne(ctx); err != nil {
			slog.Error("worker process failed", "err", err, "stage", w.cfg.Stage, "worker_id", w.cfg.WorkerID)
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (didWork bool, err error) {
	rawID, err := w.cfg.Queue.BlockingPop(ctx, w.cfg.Stage, w.cfg.PopTimeout)
	if errors.Is(err, queue.ErrEmpty) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pop: %w", err)
	}

	jobID, err := uuid.Parse(rawID)
	if err != nil {
		return true, fmt.Errorf("parse job id %q: %w", rawID, err)
	}

	if err := w.cfg.Store.ClaimJob(ctx, jobID, w.cfg.WorkerID); err != nil {
		if errors.Is(err, store.ErrJobNotClaimable) {
			slog.Info("claim race lost", "job_id", jobID, "worker_id", w.cfg.WorkerID)
			return true, nil
		}
		return true, fmt.Errorf("claim: %w", err)
	}

	job, err := w.cfg.Store.GetJob(ctx, jobID)
	if err != nil {
		return true, fmt.Errorf("get: %w", err)
	}

	hbCtx, hbCancel := context.WithCancel(ctx)
	go w.heartbeatLoop(hbCtx)

	handlerErr := w.cfg.Handler.Process(ctx, job)

	hbCancel()

	if handlerErr != nil {
		nextRun := time.Now().Add(Compute(job.Attempts + 1))
		if err := w.cfg.Store.MarkFailed(ctx, jobID, handlerErr.Error(), nextRun); err != nil {
			return true, fmt.Errorf("mark failed: %w", err)
		}
		slog.Info("job failed, will retry", "job_id", jobID, "attempts", job.Attempts+1, "next_run", nextRun)
		return true, nil
	}

	if err := w.cfg.Store.MarkDone(ctx, jobID); err != nil {
		return true, fmt.Errorf("mark done: %w", err)
	}
	return true, nil
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	tick := time.NewTicker(w.cfg.HeartbeatInterval)
	defer tick.Stop()

	if err := w.cfg.Queue.Heartbeat(ctx, w.cfg.WorkerID, w.cfg.HeartbeatTTL); err != nil {
		slog.Warn("heartbeat write failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := w.cfg.Queue.Heartbeat(ctx, w.cfg.WorkerID, w.cfg.HeartbeatTTL); err != nil {
				slog.Warn("heartbeat write failed", "err", err)
			}
		}
	}
}
```

Run — expect 3 ProcessOne integration tests pass.

Commit: `feat(worker): add Worker with ProcessOne, claim, heartbeat goroutine, retry`

---

## Section E — Heartbeat liveness integration test

### Task E1: Test that heartbeat fires during long-running job

**Files:**
- Create: `internal/worker/heartbeat_integration_test.go`

- [ ] **Step 1**

```go
//go:build integration

package worker_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/worker"
)

func TestProcessOne_WritesHeartbeatDuringHandler(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx := context.Background()

	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"sleepMs":1500}`)})
	_ = h.queue.Push(ctx, "test", job.ID.String())

	done := make(chan struct{})
	go func() {
		_, _ = h.w.ProcessOne(ctx)
		close(done)
	}()

	time.Sleep(800 * time.Millisecond)

	val, err := h.queue.Client().Get(ctx, "heartbeat:worker-test-1").Result()
	if err != nil {
		t.Fatalf("heartbeat get: %v", err)
	}
	if val == "" {
		t.Fatal("heartbeat not set during handler execution")
	}

	<-done
}
```

Run — expect 1 new test pass.

Commit: `test(worker): assert heartbeat writes during long-running handler`

---

## Section F — cmd/worker main

### Task F1: Wire main.go

**Files:**
- Create: `cmd/worker/main.go`

- [ ] **Step 1: Write**

```go
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/worker"
)

func main() {
	stage := flag.String("stage", "", "queue stage name (required)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *stage == "" {
		slog.Error("--stage is required")
		os.Exit(2)
	}

	dsn := envOr("DATABASE_URL", "postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, dsn); err != nil {
		slog.Error("migrate failed", "err", err)
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

	w := worker.New(worker.Config{
		Stage:             *stage,
		WorkerID:          worker.NewWorkerID(),
		Store:             store.New(pool),
		Queue:             queue.New(redis),
		Handler:           worker.NoopHandler{},
		PopTimeout:        30 * time.Second,
		HeartbeatTTL:      15 * time.Second,
		HeartbeatInterval: 5 * time.Second,
	})

	slog.Info("worker starting", "stage", *stage)
	if err := w.Run(ctx); err != nil {
		slog.Error("worker run", "err", err)
		os.Exit(1)
	}
	slog.Info("worker stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
```

- [ ] **Step 2: Build**

```bash
go build ./cmd/worker
```

Expected: silent success.

- [ ] **Step 3: Quick smoke run (manual)**

```bash
docker compose up -d postgres redis
sleep 5
DATABASE_URL='postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable' \
REDIS_URL='redis://localhost:6379/0' \
./cmd/worker --stage=test &
WORKER_PID=$!
sleep 2
kill -TERM $WORKER_PID
wait $WORKER_PID 2>/dev/null || true
docker compose down -v
```

Expected: worker logs `worker starting`, then `worker stopped` on SIGTERM.

Commit: `feat(worker): add cmd/worker main with --stage flag and graceful shutdown`

---

## Section G — Local sweep + PR + tag

### Task G1: Local sweep

```bash
make lint
make test-unit
make test-integration
make k8s-validate
```

All exit 0.

### Task G2: Push, PR, merge, tag, release, cleanup

```bash
git push -u origin phase-2c-worker

gh pr create --base main --head phase-2c-worker \
  --title "feat(worker): Phase 2C — Worker scaffold (claim, heartbeat, retry)" \
  --body "$(cat <<'EOF'
## Summary

Phase 2C: the unit that ties store + queue together.

- `internal/worker`: pure helpers (`Compute` exponential backoff with jitter, `NewWorkerID`, `Handler` interface, `NoopHandler`) plus the `Worker` itself with `ProcessOne` and `Run`.
- `Worker.ProcessOne`: pop job ID -> claim in PG -> spawn heartbeat goroutine -> run handler -> mark done or mark failed (with backoff-computed next_run_at).
- `internal/queue`: adds `Heartbeat(workerID, ttl)` writer and `Client()` accessor (sweeper reader lands in 2D).
- `cmd/worker`: single binary, `--stage=<name>` flag, slog JSON, graceful shutdown.

8 new tests (4 unit + 3 ProcessOne integration + 1 heartbeat integration). +1 in queue pkg.

Real fetch/render/upload handlers are Phase 3. Sweeper for orphaned jobs is Phase 2D.

## Test plan

- [x] make lint
- [x] make test-unit
- [x] make test-integration
- [x] make k8s-validate
- [ ] CI green
EOF
)"

PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-2c -m "Phase 2C: Worker scaffold with claim, heartbeat, backoff"
git push origin phase-2c
gh release create phase-2c --title "Phase 2C: Worker Scaffold" \
  --notes "Worker binary that ties store + queue. Claim, heartbeat goroutine, exponential backoff retries. NoopHandler for end-to-end smoke; real handlers in Phase 3."
git push origin --delete phase-2c-worker
git branch -D phase-2c-worker
```

---

## Out of scope

- Sweeper for orphaned jobs (Phase 2D)
- Delayed-queue ZSET (Phase 2D)
- Real Gmail / Drive / PDF handlers (Phase 3)
- 5K-jobs-in-10s load test (Phase 2E)
- HPA on queue depth (Phase 1.5)
