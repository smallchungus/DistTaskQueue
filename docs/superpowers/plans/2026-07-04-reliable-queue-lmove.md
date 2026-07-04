# Reliable Queue Pop (BLMOVE) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the pop-to-claim crash window by replacing BRPOP with BLMOVE into a per-worker processing list, acked after the job's DB state is settled and swept back to the stage queue when the worker dies.

**Architecture:** `BlockingPop` becomes `BLMOVE queue:<stage> → processing:<stage>:<worker_id>`, so a popped job ID survives in Redis until the worker acks it with `LREM`. Worker heartbeats move from per-job goroutines to one process-lifetime loop started in `Run`, so the sweeper can judge liveness of a worker that crashed between pop and claim. The sweeper gains a first pass that scans `processing:*` keys and moves entries of dead workers back onto their stage queue. Duplicate pushes remain safe: the conditional `UPDATE … WHERE status='queued'` claim is still the only mutex.

**Tech Stack:** go-redis/v9 (`BLMove`, `LMove`, `LRem`, `Scan`), pgx/v5, testcontainers-go integration tests.

## Global Constraints

- TDD, red → green, per project CLAUDE.md. Integration tests use real Redis/Postgres via `testcontainers-go` (build tag `integration`); never mock go-redis or SQL.
- No queue libraries — raw Redis commands only.
- Spec before code: the design spec is updated in Task 1, before any implementation.
- Minimal diffs; do not touch `repushStale`, `promoteDelayed`, or `reviveOrphans` logic.
- Test names short, declarative (`TestSweepOnce_ReclaimsProcessingOfDeadWorker`). No comments narrating tests.
- Commit subjects: Conventional Commits, scopes `queue`/`worker`/`sweeper`, imperative, <72 chars, no trailing period.
- Run integration tests with: `go test -race -count=1 -tags=integration ./internal/<pkg>/...` (Docker required).

---

### Task 1: Spec update

**Files:**
- Modify: `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md:111-120` (data flow) and `:200-207` (Redis keys)

**Interfaces:**
- Produces: the normative description Tasks 2–4 implement (key name `processing:<stage>:<worker_id>`, ack = `LREM … 1 <job_id>`, process-lifetime heartbeat).

- [ ] **Step 1: Rewrite data-flow step 2**

Replace lines 111–117 (the `2. **Fetch worker:** …` block ending `6. Stop heartbeat. Worker loops back to BLPOP.`) with:

```markdown
2. **Fetch worker:** `BLMOVE queue:fetch processing:fetch:<worker_id> RIGHT LEFT` with 30 s timeout. The pop is a move, not a removal: until the worker acks, the job ID survives in the worker's processing list. On hit:
   1. Atomically claim job in Postgres (`UPDATE pipeline_jobs SET status='running', worker_id=…, claimed_at=now() WHERE id=$1 AND status='queued' RETURNING …`).
   2. Fetch Gmail message in `format=raw`.
   3. Write MIME bytes to `/data/mime/<job_id>.eml`.
   4. Update job: `stage='render', status='queued'`. `LPUSH queue:render <job_id>`.
   5. Ack: `LREM processing:fetch:<worker_id> 1 <job_id>`. Worker loops back to BLMOVE.

   Workers heartbeat for their whole process lifetime — `heartbeat:<worker_id>` written every 5 s with TTL 15 s, starting before the first pop — not per job. A worker that dies at any point after the pop leaves the job ID in its processing list until the ack; once its heartbeat expires the sweeper moves the entry back to the stage queue.
```

- [ ] **Step 2: Extend the crash story (step 5)**

Replace line 120 (`5. **At any stage, if the worker crashes:** …`) with:

```markdown
5. **At any stage, if the worker crashes:** Heartbeat TTL expires. If the job was claimed (`status='running'`), the sweeper increments `attempts`, schedules requeue for `now() + backoff(attempts)`, sets `status='queued'`. If the worker died between pop and claim, the job ID is still in `processing:<stage>:<worker_id>`; the sweeper moves it back to `queue:<stage>`. Both paths can push a duplicate ID; the conditional claim absorbs it.
```

- [ ] **Step 3: Update the Redis keys table (§5.2)**

Replace the first table row (line 202) and add a new row after it:

```markdown
| `queue:fetch`, `queue:render`, `queue:upload` | LIST | Per-stage job queues. Workers `BLMOVE` into their processing list. |
| `processing:<stage>:<worker_id>` | LIST | In-flight pop guard. Emptied by worker ack (`LREM`) or swept back to the stage queue once the worker's heartbeat is gone. |
```

- [ ] **Step 4: Commit**

```bash
git add docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md
git commit -m "docs: spec reliable pop via BLMOVE and process-lifetime heartbeat"
```

---

### Task 2: Queue — BLMOVE pop, Ack, ListProcessing, ReclaimProcessing

**Files:**
- Modify: `internal/queue/queue.go`
- Test: `internal/queue/queue_integration_test.go`
- Modify (mechanical, keeps `-tags=integration` compiling after the signature change): `internal/sweeper/sweeper_integration_test.go:95,123`, `internal/worker/worker_integration_test.go:164`

**Interfaces:**
- Produces:
  - `func (q *Queue) BlockingPop(ctx context.Context, stage, workerID string, timeout time.Duration) (string, error)` — BLMOVE `queue:<stage>` RIGHT → `processing:<stage>:<workerID>` LEFT; `ErrEmpty` on timeout.
  - `func (q *Queue) Ack(ctx context.Context, stage, workerID, jobID string) error` — `LREM processing:<stage>:<workerID> 1 jobID`.
  - `type ProcessingRef struct { Stage, WorkerID string }`
  - `func (q *Queue) ListProcessing(ctx context.Context) ([]ProcessingRef, error)` — SCAN `processing:*`.
  - `func (q *Queue) ReclaimProcessing(ctx context.Context, stage, workerID string) (int, error)` — LMOVE each entry back to `queue:<stage>` RIGHT (pop end, so reclaimed jobs go next).

- [ ] **Step 1: Write the failing tests**

Append to `internal/queue/queue_integration_test.go`:

```go
func TestBlockingPop_MovesJobToProcessingList(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	if err := q.Push(ctx, "fetch", "job-1"); err != nil {
		t.Fatal(err)
	}
	got, err := q.BlockingPop(ctx, "fetch", "w1", time.Second)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if got != "job-1" {
		t.Fatalf("got %q, want %q", got, "job-1")
	}

	entries, err := q.Client().LRange(ctx, "processing:fetch:w1", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != "job-1" {
		t.Fatalf("processing list: %v, want [job-1]", entries)
	}
}

func TestAck_RemovesJobFromProcessingList(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	_ = q.Push(ctx, "fetch", "job-1")
	if _, err := q.BlockingPop(ctx, "fetch", "w1", time.Second); err != nil {
		t.Fatal(err)
	}

	if err := q.Ack(ctx, "fetch", "w1", "job-1"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	n, err := q.Client().LLen(ctx, "processing:fetch:w1").Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("processing depth: %d, want 0", n)
	}
}

func TestListProcessing_ReturnsStageAndWorker(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	_ = q.Push(ctx, "fetch", "job-1")
	if _, err := q.BlockingPop(ctx, "fetch", "w1", time.Second); err != nil {
		t.Fatal(err)
	}

	refs, err := q.ListProcessing(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].Stage != "fetch" || refs[0].WorkerID != "w1" {
		t.Fatalf("refs: %+v, want [{fetch w1}]", refs)
	}
}

func TestReclaimProcessing_MovesJobsBackToQueue(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	_ = q.Push(ctx, "fetch", "job-1")
	if _, err := q.BlockingPop(ctx, "fetch", "w1", time.Second); err != nil {
		t.Fatal(err)
	}

	n, err := q.ReclaimProcessing(ctx, "fetch", "w1")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed: %d, want 1", n)
	}
	depth, _ := q.Depth(ctx, "fetch")
	if depth != 1 {
		t.Fatalf("queue depth: %d, want 1", depth)
	}
	left, _ := q.Client().LLen(ctx, "processing:fetch:w1").Result()
	if left != 0 {
		t.Fatalf("processing depth: %d, want 0", left)
	}
}
```

Update the three existing `BlockingPop` calls in this file to the new signature:
- line 48: `q.BlockingPop(ctx, "fetch", "w1", 2*time.Second)`
- line 67: `q.BlockingPop(ctx, "fetch", "w1", time.Second)`
- line 79: `q.BlockingPop(context.Background(), "fetch", "w1", 200*time.Millisecond)`

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race -count=1 -tags=integration ./internal/queue/... -run 'TestBlockingPop|TestAck|TestListProcessing|TestReclaimProcessing' -v`
Expected: compile error (`not enough arguments in call to q.BlockingPop`; `q.Ack undefined`).

- [ ] **Step 3: Implement**

In `internal/queue/queue.go`, add `"strings"` to imports, add below `key`:

```go
func processingKey(stage, workerID string) string {
	return "processing:" + stage + ":" + workerID
}
```

Replace `BlockingPop`:

```go
func (q *Queue) BlockingPop(ctx context.Context, stage, workerID string, timeout time.Duration) (string, error) {
	res, err := q.cli.BLMove(ctx, key(stage), processingKey(stage, workerID), "RIGHT", "LEFT", timeout).Result()
	if errors.Is(err, goredis.Nil) {
		return "", ErrEmpty
	}
	if err != nil {
		return "", fmt.Errorf("pop: %w", err)
	}
	return res, nil
}
```

Add:

```go
func (q *Queue) Ack(ctx context.Context, stage, workerID, jobID string) error {
	if err := q.cli.LRem(ctx, processingKey(stage, workerID), 1, jobID).Err(); err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	return nil
}

type ProcessingRef struct {
	Stage    string
	WorkerID string
}

func (q *Queue) ListProcessing(ctx context.Context) ([]ProcessingRef, error) {
	var out []ProcessingRef
	var cursor uint64
	for {
		keys, next, err := q.cli.Scan(ctx, cursor, "processing:*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan processing: %w", err)
		}
		for _, k := range keys {
			parts := strings.SplitN(k, ":", 3)
			if len(parts) != 3 {
				continue
			}
			out = append(out, ProcessingRef{Stage: parts[1], WorkerID: parts[2]})
		}
		cursor = next
		if cursor == 0 {
			return out, nil
		}
	}
}

func (q *Queue) ReclaimProcessing(ctx context.Context, stage, workerID string) (int, error) {
	moved := 0
	for {
		_, err := q.cli.LMove(ctx, processingKey(stage, workerID), key(stage), "RIGHT", "RIGHT").Result()
		if errors.Is(err, goredis.Nil) {
			return moved, nil
		}
		if err != nil {
			return moved, fmt.Errorf("reclaim: %w", err)
		}
		moved++
	}
}
```

Mechanical call-site updates in other packages' tests (workerID argument only; behavior unchanged):
- `internal/sweeper/sweeper_integration_test.go:95` → `q.BlockingPop(ctx, "test", "w-verify", 1*time.Second)`
- `internal/sweeper/sweeper_integration_test.go:123` → `q.BlockingPop(ctx, "fetch", "w-verify", 1*time.Second)`
- `internal/worker/worker_integration_test.go:164` → `h.queue.BlockingPop(ctx, "render", "w-verify", 500*time.Millisecond)`

Note: `internal/worker/worker.go:48` also calls `BlockingPop` and will not compile until Task 3. Make the one-line signature fix there now (`w.cfg.Queue.BlockingPop(ctx, w.cfg.Stage, w.cfg.WorkerID, w.cfg.PopTimeout)`) — the ack/heartbeat behavior changes stay in Task 3.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 -tags=integration ./internal/queue/... -v`
Expected: PASS, including the four new tests and updated existing pop tests.

Run: `go build ./... && go vet ./...`
Expected: clean.

- [ ] **Step 5: Commit**

```bash
git add internal/queue internal/worker internal/sweeper
git commit -m "feat(queue): reliable pop via BLMOVE with processing-list ack and reclaim"
```

---

### Task 3: Worker — ack after settle, process-lifetime heartbeat

**Files:**
- Modify: `internal/worker/worker.go:34-125`
- Test: `internal/worker/worker_integration_test.go`

**Interfaces:**
- Consumes: `Queue.BlockingPop(ctx, stage, workerID, timeout)`, `Queue.Ack(ctx, stage, workerID, jobID)`, `Queue.Heartbeat(ctx, workerID, ttl)` from Task 2.
- Produces: `Worker.Run` heartbeats for the whole process lifetime; `Worker.ProcessOne` acks the processing-list entry on every return path after a successful pop. Public signatures unchanged.

- [ ] **Step 1: Write the failing tests**

Append to `internal/worker/worker_integration_test.go`:

```go
func TestProcessOne_AcksProcessingList(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx := context.Background()

	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = h.queue.Push(ctx, "test", job.ID.String())

	if _, err := h.w.ProcessOne(ctx); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}

	n, err := h.queue.Client().LLen(ctx, "processing:test:worker-test-1").Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("processing depth: %d, want 0", n)
	}
}

func TestProcessOne_AcksWhenClaimRaceLost(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx := context.Background()

	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	if err := h.store.ClaimJob(ctx, job.ID, "rival-worker"); err != nil {
		t.Fatal(err)
	}
	_ = h.queue.Push(ctx, "test", job.ID.String())

	didWork, err := h.w.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if !didWork {
		t.Fatal("expected didWork=true")
	}
	n, _ := h.queue.Client().LLen(ctx, "processing:test:worker-test-1").Result()
	if n != 0 {
		t.Fatalf("processing depth: %d, want 0", n)
	}
}

func TestRun_HeartbeatsWhileIdle(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = h.w.Run(ctx); close(done) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		alive, err := h.queue.IsWorkerAlive(context.Background(), "worker-test-1")
		if err != nil {
			t.Fatal(err)
		}
		if alive {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no heartbeat within 2s of Run starting")
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race -count=1 -tags=integration ./internal/worker/... -run 'TestProcessOne_Acks|TestRun_Heartbeats' -v`
Expected: `TestProcessOne_AcksProcessingList` and `TestProcessOne_AcksWhenClaimRaceLost` FAIL (`processing depth: 1, want 0`). `TestRun_HeartbeatsWhileIdle` FAILs on the 2 s deadline (heartbeat currently only written while a job is in flight).

- [ ] **Step 3: Implement**

In `internal/worker/worker.go`, replace `Run`:

```go
func (w *Worker) Run(ctx context.Context) error {
	if err := w.cfg.Queue.Heartbeat(ctx, w.cfg.WorkerID, w.cfg.HeartbeatTTL); err != nil {
		slog.Warn("heartbeat write failed", "err", err)
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go w.heartbeatLoop(hbCtx)

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
```

In `ProcessOne`, after the pop error handling (post line 54), add the ack and delete the per-job heartbeat lines (74–75 `hbCtx, hbCancel := …` / `go w.heartbeatLoop(hbCtx)` and 79 `hbCancel()`):

```go
	defer func() {
		if ackErr := w.cfg.Queue.Ack(context.WithoutCancel(ctx), w.cfg.Stage, w.cfg.WorkerID, rawID); ackErr != nil {
			slog.Warn("ack failed", "job_id", rawID, "err", ackErr)
		}
	}()
```

`heartbeatLoop` itself is unchanged. `context.WithoutCancel` keeps the ack working during graceful shutdown; if the process dies outright the sweeper reclaims the entry.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 -tags=integration ./internal/worker/... -v`
Expected: PASS (all existing + 3 new).

- [ ] **Step 5: Commit**

```bash
git add internal/worker
git commit -m "feat(worker): ack processing entry and heartbeat for process lifetime"
```

---

### Task 4: Sweeper — reclaim dead workers' processing lists

**Files:**
- Modify: `internal/sweeper/sweeper.go:50-61` (SweepOnce) + new method
- Test: `internal/sweeper/sweeper_integration_test.go`

**Interfaces:**
- Consumes: `Queue.ListProcessing`, `Queue.ReclaimProcessing`, `Queue.IsWorkerAlive` from Task 2.
- Produces: `SweepOnce` runs `reclaimProcessing` as its first pass.

- [ ] **Step 1: Write the failing tests**

Append to `internal/sweeper/sweeper_integration_test.go`:

```go
func TestSweepOnce_ReclaimsProcessingOfDeadWorker(t *testing.T) {
	_, q, sw := setup(t)
	ctx := context.Background()

	_ = q.Push(ctx, "test", "job-lost")
	if _, err := q.BlockingPop(ctx, "test", "w-dead", time.Second); err != nil {
		t.Fatal(err)
	}

	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	got, err := q.BlockingPop(ctx, "test", "w-verify", time.Second)
	if err != nil {
		t.Fatalf("expected job back on queue: %v", err)
	}
	if got != "job-lost" {
		t.Fatalf("got %q, want %q", got, "job-lost")
	}
}

func TestSweepOnce_LeavesProcessingOfLiveWorker(t *testing.T) {
	_, q, sw := setup(t)
	ctx := context.Background()

	if err := q.Heartbeat(ctx, "w-alive", 30*time.Second); err != nil {
		t.Fatal(err)
	}
	_ = q.Push(ctx, "test", "job-inflight")
	if _, err := q.BlockingPop(ctx, "test", "w-alive", time.Second); err != nil {
		t.Fatal(err)
	}

	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}

	depth, _ := q.Depth(ctx, "test")
	if depth != 0 {
		t.Fatalf("queue depth: %d, want 0 (job stays in processing)", depth)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test -race -count=1 -tags=integration ./internal/sweeper/... -run 'TestSweepOnce_ReclaimsProcessing|TestSweepOnce_LeavesProcessing' -v`
Expected: `TestSweepOnce_ReclaimsProcessingOfDeadWorker` FAILs with `expected job back on queue: queue empty`. `TestSweepOnce_LeavesProcessingOfLiveWorker` passes trivially (nothing reclaims yet) — that's fine, it guards the implementation.

- [ ] **Step 3: Implement**

In `internal/sweeper/sweeper.go`, make `reclaimProcessing` the first pass of `SweepOnce`:

```go
func (s *Sweeper) SweepOnce(ctx context.Context) error {
	if err := s.reclaimProcessing(ctx); err != nil {
		return fmt.Errorf("reclaim processing: %w", err)
	}
	if err := s.reviveOrphans(ctx); err != nil {
		return fmt.Errorf("revive orphans: %w", err)
	}
	if err := s.promoteDelayed(ctx); err != nil {
		return fmt.Errorf("promote delayed: %w", err)
	}
	if err := s.repushStale(ctx); err != nil {
		return fmt.Errorf("repush stale: %w", err)
	}
	return nil
}

func (s *Sweeper) reclaimProcessing(ctx context.Context) error {
	refs, err := s.cfg.Queue.ListProcessing(ctx)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		alive, err := s.cfg.Queue.IsWorkerAlive(ctx, ref.WorkerID)
		if err != nil {
			slog.Warn("heartbeat check failed", "worker_id", ref.WorkerID, "err", err)
			continue
		}
		if alive {
			continue
		}
		n, err := s.cfg.Queue.ReclaimProcessing(ctx, ref.Stage, ref.WorkerID)
		if err != nil {
			slog.Warn("reclaim failed", "worker_id", ref.WorkerID, "stage", ref.Stage, "err", err)
			continue
		}
		if n > 0 {
			slog.Info("reclaimed processing jobs", "worker_id", ref.WorkerID, "stage", ref.Stage, "count", n)
		}
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race -count=1 -tags=integration ./internal/sweeper/... -v`
Expected: PASS (all existing + 2 new).

- [ ] **Step 5: Commit**

```bash
git add internal/sweeper
git commit -m "feat(sweeper): reclaim processing lists of dead workers"
```

---

### Task 5: ARCHITECTURE.md update

**Files:**
- Modify: `docs/ARCHITECTURE.md:24,48-52,109-122,234-255,274-288`

ARCHITECTURE.md describes the running system, so it changes after the code. Update each BLPOP-era passage:

- [ ] **Step 1: Update the overview (line 24)**

Replace `` `BLPOP` an ID off the queue, `` with `` `BLMOVE` an ID into a per-worker processing list, `` and replace `Workers write a short-lived heartbeat key to Redis every 5 s while processing.` with `Workers write a short-lived heartbeat key to Redis every 5 s for their whole process lifetime.` In the sweeper sentence, replace `(c) queued jobs without last_error that have been sitting too long → re-push (recovers BLPOP-to-claim crashes)` with `(c) queued jobs without last_error that have been sitting too long → re-push (recovers lost pushes), (d) processing-list entries of dead workers → moved back to the stage queue (recovers pop-to-claim crashes)`.

- [ ] **Step 2: Update the worker walkthrough (lines 109–122)**

Replace list items 1, 3, and 6:

```markdown
1. `queue.BlockingPop(ctx, "fetch", workerID, 30s)` → `BLMOVE queue:fetch processing:fetch:<workerID> RIGHT LEFT`. Blocking, so workers idle without burning CPU — and the popped ID survives in the processing list until the worker acks, so a crash between pop and claim loses nothing.
3. (deleted — heartbeats are process-lifetime, started once in `Run`)
6. Ack: `LREM processing:fetch:<workerID> 1 <jobID>` (deferred — runs on every settle path: done, advanced, failed, claim race lost). Loop back to BLMOVE.
```

Renumber the list accordingly (2, 3, 4, 5 stay in order).

- [ ] **Step 3: Update the pseudocode and shutdown note (lines 234–255)**

In the worker-loop pseudocode, replace the `hbCtx` / `go heartbeatLoop` / `cancel()` lines with a `defer queue.Ack(...)` after the pop and add `go heartbeatLoop(ctx)` before the loop. In the graceful-shutdown paragraph, replace `BLPOP returns quickly` with `BLMOVE returns quickly`.

- [ ] **Step 4: Update the sweeper passes (lines 274–288)**

Change "Three passes per sweep" to "Four passes per sweep" and insert as the new pass 1:

```markdown
1. **Processing reclaim.** `SCAN processing:*`. For each `processing:<stage>:<worker_id>` list whose worker heartbeat is absent, `LMOVE` every entry back to `queue:<stage>`. Recovers jobs whose worker died after popping but before acking.
```

Renumber the existing three passes to 2–4. In the old pass 3 (stale-queued revival), replace the second sentence with: `Recovers jobs whose Redis push never landed or got lost — the pop-to-claim crash is now covered by the processing reclaim, so this pass is the backstop for missing pushes.` Keep the "Duplicate pushes are safe" paragraph; replace `BLPOP-ing` with `popping`.

- [ ] **Step 5: Commit**

```bash
git add docs/ARCHITECTURE.md
git commit -m "docs: architecture reflects BLMOVE pop, ack, and processing reclaim"
```

---

### Task 6: Full verification

- [ ] **Step 1: Run everything**

Run: `make test` (unit + integration) and `make loadtest`
Expected: all `ok`, and the loadtest still finishes 5000 jobs / 4 workers under 15 s (BLMOVE adds no extra round trip vs BRPOP; the ack adds one O(1) LREM per job).

- [ ] **Step 2: Lint**

Run: `make lint`
Expected: clean.

Paste actual outputs in the completion report — per project rules, `ok internal/queue 0.4s` is evidence, "tests pass" is not.
