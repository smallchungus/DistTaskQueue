# Phase 2B — Redis Queue Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the ephemeral hand-off layer for the queue — a tiny Redis-backed `Queue` type that supports `Push`, `BlockingPop`, and `Depth` per stage. No worker logic, no claim semantics, no sweeper — those are 2C and 2D.

**Architecture:** `internal/queue` wraps a `*goredis.Client`. Per-stage queues are Redis lists keyed `queue:<stage>` (e.g., `queue:fetch`, `queue:render`). `Push` does `LPUSH`, `BlockingPop` does `BLPOP`, `Depth` does `LLEN`. The job ID flowing through Redis is the Postgres `pipeline_jobs.id` (UUID string). Everything else lives in Postgres.

**Tech Stack:** `go-redis/v9` (already a direct dep from Phase 2A), existing testcontainers `StartRedis` helper.

**Spec reference:** `docs/superpowers/specs/2026-04-17-distributed-task-queue-design.md` Section 5.2.

**Branch:** `phase-2b-redis-queue` → PR to `main`.

---

## Section A — Branch + Push (TDD)

### Task A1: Branch

- [ ] **Step 1: Branch off main**

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-2b-redis-queue
```

### Task A2: Failing test for Push

**Files:**
- Create: `internal/queue/queue_integration_test.go`

- [ ] **Step 1: Write the test**

```go
//go:build integration

package queue_test

import (
	"context"
	"testing"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func newQueue(t *testing.T) *queue.Queue {
	t.Helper()
	cli := testutil.StartRedis(t)
	return queue.New(cli)
}

func TestPush_AppendsToStageList(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	if err := q.Push(ctx, "fetch", "job-1"); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := q.Push(ctx, "fetch", "job-2"); err != nil {
		t.Fatalf("push: %v", err)
	}

	depth, err := q.Depth(ctx, "fetch")
	if err != nil {
		t.Fatalf("depth: %v", err)
	}
	if depth != 2 {
		t.Fatalf("depth: got %d, want 2", depth)
	}
}
```

- [ ] **Step 2: Run, expect failure**

```bash
go test -tags=integration ./internal/queue/...
```

Expected: undefined `queue.Queue`, `queue.New`, `q.Push`, `q.Depth`.

### Task A3: Implement Queue with Push and Depth

**Files:**
- Create: `internal/queue/queue.go`

- [ ] **Step 1: Write Queue**

```go
package queue

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

type Queue struct {
	cli *goredis.Client
}

func New(cli *goredis.Client) *Queue {
	return &Queue{cli: cli}
}

func key(stage string) string {
	return "queue:" + stage
}

func (q *Queue) Push(ctx context.Context, stage, jobID string) error {
	if err := q.cli.LPush(ctx, key(stage), jobID).Err(); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func (q *Queue) Depth(ctx context.Context, stage string) (int64, error) {
	n, err := q.cli.LLen(ctx, key(stage)).Result()
	if err != nil {
		return 0, fmt.Errorf("depth: %w", err)
	}
	return n, nil
}
```

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/queue/...
```

Expected: 1 test pass.

- [ ] **Step 3: Commit**

```bash
git add internal/queue/
git commit -m "feat(queue): add Queue with Push and Depth"
```

---

## Section B — BlockingPop (TDD)

### Task B1: Failing test

**Files:**
- Modify: `internal/queue/queue_integration_test.go`

- [ ] **Step 1: Append**

```go
import (
	"context"
	"testing"
	"time"
	// existing imports
)

func TestBlockingPop_ReturnsPushedJobID(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	if err := q.Push(ctx, "fetch", "job-abc"); err != nil {
		t.Fatal(err)
	}

	jobID, err := q.BlockingPop(ctx, "fetch", 2*time.Second)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if jobID != "job-abc" {
		t.Fatalf("jobID: got %q, want %q", jobID, "job-abc")
	}
}

func TestBlockingPop_FIFOOrdering(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Push(ctx, "fetch", id); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []string{"a", "b", "c"} {
		got, err := q.BlockingPop(ctx, "fetch", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestBlockingPop_ReturnsErrEmptyOnTimeout(t *testing.T) {
	q := newQueue(t)
	_, err := q.BlockingPop(context.Background(), "fetch", 200*time.Millisecond)
	if err != queue.ErrEmpty {
		t.Fatalf("got %v, want ErrEmpty", err)
	}
}
```

You'll need to add `"time"` to imports.

- [ ] **Step 2: Run, expect failure**

Expected: undefined `q.BlockingPop`, `queue.ErrEmpty`.

### Task B2: Implement BlockingPop

**Files:**
- Modify: `internal/queue/queue.go`

- [ ] **Step 1: Append**

```go
import (
	"context"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

var ErrEmpty = errors.New("queue empty")

func (q *Queue) BlockingPop(ctx context.Context, stage string, timeout time.Duration) (string, error) {
	res, err := q.cli.BRPop(ctx, timeout, key(stage)).Result()
	if errors.Is(err, goredis.Nil) {
		return "", ErrEmpty
	}
	if err != nil {
		return "", fmt.Errorf("pop: %w", err)
	}
	if len(res) != 2 {
		return "", fmt.Errorf("pop: unexpected response shape %v", res)
	}
	return res[1], nil
}
```

Notes:
- `BRPop` (right-side blocking pop) paired with `LPush` (left-side push) gives FIFO ordering — older jobs come out first. The plan name is "BlockingPop" but the underlying op is BRPop — that's intentional, FIFO is what we want.
- `goredis.Nil` is the sentinel returned when BRPop times out without an element. Map to our `ErrEmpty`.

Update the import block to consolidate (single `import (...)` block).

- [ ] **Step 2: Run, expect pass**

```bash
go test -tags=integration ./internal/queue/...
```

Expected: 4 tests pass (Push+Depth, BlockingPop+FIFO+timeout).

- [ ] **Step 3: Commit**

```bash
git add internal/queue/
git commit -m "feat(queue): add BlockingPop (BRPop, FIFO with LPush) and ErrEmpty"
```

---

## Section C — Final verification + PR

### Task C1: Local sweep

- [ ] **Step 1: Run all checks**

```bash
make lint
make test-unit
make test-integration
make k8s-validate
```

Expected: every command exits 0. Capture each tail.

### Task C2: Push, PR, merge

- [ ] **Step 1: Push branch**

```bash
git push -u origin phase-2b-redis-queue
```

- [ ] **Step 2: Open PR**

```bash
gh pr create --base main --head phase-2b-redis-queue --title "feat(queue): Phase 2B — Redis-backed Queue (Push, BlockingPop, Depth)" --body "$(cat <<'EOF'
## Summary

Phase 2B of the queue: ephemeral hand-off layer.

- `internal/queue` wraps `*goredis.Client`.
- Methods:
  - `Push(ctx, stage, jobID)` — `LPUSH queue:<stage> <jobID>`
  - `BlockingPop(ctx, stage, timeout)` — `BRPOP queue:<stage>` (FIFO with LPush). Returns `ErrEmpty` on timeout.
  - `Depth(ctx, stage)` — `LLEN queue:<stage>`
- 4 integration tests against real Redis via testcontainers-go.

Worker logic, claim semantics, and sweeper come in 2C / 2D.

## Test plan

- [x] make lint
- [x] make test-unit
- [x] make test-integration
- [x] make k8s-validate
- [ ] CI green
EOF
)"
```

- [ ] **Step 3: Auto-merge**

```bash
PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

- [ ] **Step 4: After merge — pull, tag, release, clean up**

```bash
git checkout main && git pull --ff-only
git tag -a phase-2b -m "Phase 2B: Redis queue — Push, BlockingPop, Depth"
git push origin phase-2b
gh release create phase-2b --title "Phase 2B: Redis Queue" --notes "Ephemeral hand-off layer: LPUSH/BRPOP-based per-stage queues with depth observation. 4 integration tests against real Redis."
git push origin --delete phase-2b-redis-queue
git branch -D phase-2b-redis-queue
```

---

## Out of scope

- Worker binary (Phase 2C)
- Heartbeats (Phase 2C)
- Claim semantics (handled in store, used by worker in 2C)
- Sweeper / delayed retries / ZSET (Phase 2D)
- HPA-driving Prometheus metric emission (Phase 1.5)
