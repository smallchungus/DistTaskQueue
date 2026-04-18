# Phase 2E — End-to-End Load Test Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax.

**Goal:** Prove the resume claim live: **"5K jobs across 4 workers in ~10 seconds."** End-to-end smoke through enqueue → Redis → claim → NoopHandler → MarkDone, exercising every primitive built in Phase 2A–2D. Reproducible with `make loadtest` against real Postgres + Redis via testcontainers-go.

**Architecture:**

```
internal/loadtest/
  loadtest_test.go    //go:build loadtest
                      Spins PG+Redis, enqueues N jobs, runs M workers
                      as goroutines, polls until done, reports elapsed,
                      asserts <15s for the default scenario (5000/4).
Makefile
  loadtest target → go test -tags=loadtest -v -timeout=2m ./internal/loadtest/...
README.md
  "Reproducing the resume numbers" section.
```

In-process workers (4 goroutines, each running `worker.Run`) on the same machine. Each goroutine has its own `WorkerID`, all coordinate via real Postgres (claim atomicity) and real Redis (queue + heartbeats). The distributed-systems primitives are exercised exactly the same as a multi-process deployment — `ClaimJob` is atomic regardless of whether the contenders share an OS process. Multi-process containerized version is a Phase 1.5 follow-up.

`PopTimeout` is set to `100ms` (vs. production's 30s) so workers spin tightly when the queue has work. `HeartbeatInterval` and `HeartbeatTTL` use small values (200ms / 1s) since the test runs for seconds, not hours.

Default scenario: **5000 jobs, 4 workers, NoopHandler with sleepMs=0**. Expected wall clock on a modern dev laptop: 5–10 s. Headroom assertion: < 15 s.

**Branch:** `phase-2e-loadtest` → PR to `main`.

---

## Section A — Branch + load test

### Task A1: Branch

- [ ] **Step 1**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-2e-loadtest
```

### Task A2: Write the load test

**Files:**
- Create: `internal/loadtest/loadtest_test.go`

- [ ] **Step 1**

Create `internal/loadtest/loadtest_test.go`:

```go
//go:build loadtest

package loadtest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
	"github.com/smallchungus/disttaskqueue/internal/worker"
)

func TestLoad_5000Jobs_4Workers_Under15s(t *testing.T) {
	const (
		jobCount    = 5000
		workerCount = 4
		stage       = "loadtest"
		deadline    = 15 * time.Second
	)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	t.Logf("enqueueing %d jobs", jobCount)
	enqueueStart := time.Now()
	jobIDs := make([]string, 0, jobCount)
	for i := 0; i < jobCount; i++ {
		j, err := s.EnqueueJob(context.Background(), store.NewJob{Stage: stage})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		jobIDs = append(jobIDs, j.ID.String())
	}
	for _, id := range jobIDs {
		if err := q.Push(context.Background(), stage, id); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	t.Logf("enqueue complete in %v", time.Since(enqueueStart))

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	processStart := time.Now()
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		w := worker.New(worker.Config{
			Stage:             stage,
			WorkerID:          fmt.Sprintf("loadtest-w%d", i),
			Store:             s,
			Queue:             q,
			Handler:           worker.NoopHandler{},
			PopTimeout:        100 * time.Millisecond,
			HeartbeatTTL:      1 * time.Second,
			HeartbeatInterval: 200 * time.Millisecond,
		})
		go func() {
			defer wg.Done()
			_ = w.Run(runCtx)
		}()
	}

	pollDeadline := time.Now().Add(deadline)
	for time.Now().Before(pollDeadline) {
		var done int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pipeline_jobs WHERE stage = $1 AND status = 'done'`,
			stage,
		).Scan(&done)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if done >= jobCount {
			elapsed := time.Since(processStart)
			t.Logf("ALL DONE: %d jobs / %d workers in %v (%.0f jobs/s)",
				jobCount, workerCount, elapsed, float64(jobCount)/elapsed.Seconds())
			cancel()
			wg.Wait()
			if elapsed > deadline {
				t.Fatalf("too slow: %v > %v", elapsed, deadline)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	var done int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pipeline_jobs WHERE stage = $1 AND status = 'done'`, stage).Scan(&done)
	cancel()
	wg.Wait()
	t.Fatalf("deadline exceeded: only %d / %d jobs done after %v", done, jobCount, deadline)
}
```

### Task A3: Verify the test fails fast (no implementation needed — it should compile and run)

- [ ] **Step 1**

```bash
cd /Users/willchen/Development/DistTaskQueue
go test -tags=loadtest -v -timeout=3m ./internal/loadtest/... 2>&1 | tail -20
```

Expected: test PASSES with a log line like `ALL DONE: 5000 jobs / 4 workers in 7.2s (694 jobs/s)`. The wall-clock value will vary by machine but should be well under 15s on any modern laptop.

If it fails (e.g. timeout exceeded), capture full output and report — that means we have a perf bug to fix before claiming the resume number, and that's a real finding.

### Task A4: Add Makefile target

**Files:**
- Modify: `Makefile`

- [ ] **Step 1**

Append `loadtest` to the `.PHONY` line:

```
.PHONY: help test test-unit test-integration lint build run docker docker-up docker-down k8s-validate install-hooks loadtest
```

Add a target near `test-integration`:

```makefile
loadtest: ## Run end-to-end load test (5K jobs / 4 workers / <15s, requires Docker)
	go test -tags=loadtest -v -timeout=2m ./internal/loadtest/...
```

(TAB indentation on the recipe.)

Verify:

```bash
make help | grep loadtest
make loadtest 2>&1 | tail -10
```

### Task A5: Commit

```bash
git add internal/loadtest/ Makefile
git commit -m "feat(loadtest): add 5000-job 4-worker end-to-end load test"
```

---

## Section B — README "Reproducing the resume numbers"

### Task B1: Update README

**Files:**
- Modify: `README.md`

- [ ] **Step 1**

Add a new section just before `## Architecture`:

```markdown
## Reproducing the resume numbers

The resume claim "5K jobs across 4 workers in ~10 seconds" is reproducible:

```bash
make loadtest
```

This spins up real Postgres + Redis containers via testcontainers-go, enqueues 5000 no-op jobs into the Redis queue, runs 4 workers in parallel, and reports wall-clock time. The test asserts under 15 s; a modern laptop typically completes in 5–10 s.

What's actually exercised:

- `pipeline_jobs` row inserted per job (`store.EnqueueJob`)
- `LPUSH` per job (`queue.Push`)
- `BRPOP` per worker per job (`queue.BlockingPop`)
- Atomic claim via Postgres `UPDATE … RETURNING` (`store.ClaimJob`)
- Heartbeat goroutine writing `SET heartbeat:<id> EX 1s` every 200 ms during processing
- `MarkDone` → `UPDATE … status='done', completed_at=now()` per job
- Polling loop until `count(status=done) == 5000`

Workers are goroutines in the same OS process. The atomic primitives (claim, heartbeat) work identically to a multi-process deployment because contention happens at Postgres and Redis, not in Go memory. A multi-process containerized version ships in Phase 1.5.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs: add reproducing-resume-numbers section to README"
```

---

## Section C — Local sweep + PR + tag (both phase-2e AND phase-2 milestone)

### Task C1: Sweep

```bash
make lint
make test-unit
make test-integration
make k8s-validate
make loadtest
```

All exit 0.

### Task C2: PR

```bash
git push -u origin phase-2e-loadtest

gh pr create --base main --head phase-2e-loadtest \
  --title "feat(loadtest): Phase 2E — 5K-job / 4-worker end-to-end test" \
  --body "$(cat <<'EOF'
## Summary

Phase 2E: closes Phase 2 by proving the resume claim live.

- New `internal/loadtest` package, behind `//go:build loadtest`. Runs 5000 NoopHandler jobs through 4 workers (in-process goroutines), asserts wall clock < 15 s.
- New `make loadtest` target.
- README section on reproducing the numbers.

What this exercises (per job × 5000): `EnqueueJob` + `Push` + `BRPOP` + `ClaimJob` + heartbeat goroutine + `MarkDone`. Plus polling loop. Real Postgres + Redis via testcontainers — no mocks.

Phase 2 is now complete. The queue handles enqueue → claim → process → heartbeat → mark done, with sweeper-driven recovery for crashed workers and exponential-backoff retries for failed handlers.

## Test plan

- [x] make lint
- [x] make test-unit
- [x] make test-integration (all packages green)
- [x] make k8s-validate
- [x] make loadtest (5000 jobs / 4 workers / under 15s)
- [ ] CI green
EOF
)"
```

```bash
PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

### Task C3: After merge — tag both phase-2e AND phase-2

```bash
git checkout main && git pull --ff-only

git tag -a phase-2e -m "Phase 2E: end-to-end load test (5K jobs / 4 workers / <15s)"
git push origin phase-2e

git tag -a phase-2 -m "Phase 2 milestone: queue core complete (store + queue + worker + sweeper + load test)"
git push origin phase-2

gh release create phase-2e --title "Phase 2E: Load Test" \
  --notes "5000 jobs across 4 workers under 15 seconds — reproducing the resume claim. make loadtest spins real Postgres + Redis via testcontainers and reports wall clock."

gh release create phase-2 --title "Phase 2: Queue Core (Milestone)" \
  --notes "Phase 2 complete. The queue handles enqueue → claim → process → heartbeat → mark done, with sweeper-driven recovery and exponential backoff retries.

Sub-phases shipped: 2A (Postgres store), 2B (Redis queue), 2C (Worker scaffold), 2D (Sweeper), 2E (Load test).

Next: Phase 1.5 (Hetzner k3s + multi-env CI/CD) and Phase 3 (Gmail → PDF → Drive workers)."

git push origin --delete phase-2e-loadtest
git branch -D phase-2e-loadtest
```

---

## Out of scope

- Real Gmail / Drive / PDF handlers (Phase 3)
- Multi-process / containerized load test (Phase 1.5)
- HPA / Prometheus queue-depth metric (Phase 1.5)
- Dashboard with live job flow (Phase 4)
