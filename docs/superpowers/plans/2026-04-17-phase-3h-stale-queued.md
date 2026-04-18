# Phase 3H — Fix BLPOP-to-claim race (stale-queued recovery)

> Use `superpowers:subagent-driven-development`. `- [ ]` checkboxes track steps.

**Goal:** Close the one remaining correctness gap. Today, if a worker crashes between `BLPOP` and `ClaimJob`, the job ID is gone from Redis but still `status='queued'` in Postgres with `last_error IS NULL`. The existing sweeper paths only handle (a) running jobs whose worker died and (b) failed jobs whose backoff has elapsed. New-but-lost jobs sit forever.

**Fix:** Extend the sweeper with a third pass — re-push any job that's been `status='queued' AND last_error IS NULL AND created_at < now() - threshold` back onto its stage queue. Threshold defaults to 60 s (longer than any normal BLPOP-to-claim window, shorter than typical user impact).

Duplicate pushes are safe — `ClaimJob` is atomic; a worker that picks up an already-claimed job ID gets `ErrJobNotClaimable` and moves on.

**Architecture:**

```
internal/store/store.go   + ListStaleQueuedJobs(threshold time.Duration)
internal/store/store_integration_test.go   + 2 tests
internal/sweeper/sweeper.go    + repushStale, Config.StaleThreshold
internal/sweeper/sweeper_integration_test.go   + 2 tests
cmd/sweeper/main.go       + read STALE_QUEUED_THRESHOLD env var
.env.example              + document STALE_QUEUED_THRESHOLD
```

**Branch:** `phase-3h-stale-queued`.

---

## Section A — Branch + ListStaleQueuedJobs

### Task A1: Branch

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3h-stale-queued
```

### Task A2: Failing test

Append to `internal/store/store_integration_test.go`:

```go
func TestListStaleQueuedJobs_ReturnsOldQueuedWithoutLastError(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// Old enough — should be returned.
	jStale, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	if _, err := s.GetJob(ctx, jStale.ID); err != nil {
		t.Fatal(err)
	}
	// Backdate created_at by 5 minutes so the threshold catches it.
	if _, err := pool(t, s).Exec(ctx,
		`UPDATE pipeline_jobs SET created_at = now() - interval '5 minutes' WHERE id = $1`,
		jStale.ID,
	); err != nil {
		t.Fatal(err)
	}

	// Fresh — should NOT be returned.
	_, _ = s.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})

	// Failed-and-retrying (last_error set) — should NOT be returned by THIS query.
	jRetry, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	_ = s.ClaimJob(ctx, jRetry.ID, "w1")
	_ = s.MarkFailed(ctx, jRetry.ID, "boom", time.Now().Add(-1*time.Hour))
	_, _ = pool(t, s).Exec(ctx,
		`UPDATE pipeline_jobs SET created_at = now() - interval '5 minutes' WHERE id = $1`,
		jRetry.ID,
	)

	jobs, err := s.ListStaleQueuedJobs(ctx, 1*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d, want 1; got: %v", len(jobs), jobIDs(jobs))
	}
	if jobs[0].ID != jStale.ID {
		t.Fatalf("got %s, want %s", jobs[0].ID, jStale.ID)
	}
}

// pool extracts the *pgxpool.Pool from a Store for tests that need raw SQL.
// The Store doesn't expose its pool by design; this helper uses a tiny
// reflective access only available to tests in the same package... but since
// store_integration_test.go is in package store_test, we instead use a small
// public test hook. Add it to store.go as PoolForTest.
func pool(t *testing.T, s *store.Store) interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
} {
	t.Helper()
	return s.PoolForTest()
}
```

The `pool` helper needs a small public test hook on Store. Add `PoolForTest()` to `store.go`:

```go
// PoolForTest exposes the underlying pool. Use only in tests that need raw
// SQL access (e.g., to backdate created_at for stale-queued tests).
func (s *Store) PoolForTest() *pgxpool.Pool { return s.pool }
```

Also add the import: `"github.com/jackc/pgx/v5/pgconn"` in the test file.

Run `go test -tags=integration ./internal/store/...` — expect undefined `s.ListStaleQueuedJobs` and undefined `s.PoolForTest`. Capture last 3 lines.

### Task A3: Implement

Append to `internal/store/store.go`:

```go
// PoolForTest exposes the underlying pool. Use only in tests that need raw
// SQL access (e.g., to backdate created_at for stale-queued tests).
func (s *Store) PoolForTest() *pgxpool.Pool { return s.pool }

// ListStaleQueuedJobs returns jobs that have been status='queued' with no
// last_error for longer than threshold. These are jobs whose Redis push
// might have been consumed by a worker that crashed before claiming them
// (the BLPOP-to-claim race) — the sweeper re-pushes them.
func (s *Store) ListStaleQueuedJobs(ctx context.Context, threshold time.Duration) ([]Job, error) {
	const q = `
		SELECT id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
		       worker_id, attempts, max_attempts, last_error, next_run_at,
		       claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs
		WHERE status = $1 AND last_error IS NULL
		  AND created_at < now() - make_interval(secs => $2)`

	rows, err := s.pool.Query(ctx, q, StatusQueued, threshold.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list stale queued: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}
```

Run integration tests — expect new test passes.

Commit: `feat(store): add ListStaleQueuedJobs and PoolForTest helper`

---

## Section B — Sweeper repushStale + Config

### Task B1: Failing tests

Append to `internal/sweeper/sweeper_integration_test.go`:

```go
func TestSweepOnce_RepushesStaleQueuedJob(t *testing.T) {
	s, q, sw := setup(t)
	ctx := context.Background()

	job, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	// Backdate so the stale threshold catches it.
	_, _ = s.PoolForTest().Exec(ctx,
		`UPDATE pipeline_jobs SET created_at = now() - interval '5 minutes' WHERE id = $1`,
		job.ID,
	)

	depthBefore, _ := q.Depth(ctx, "fetch")
	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	depthAfter, _ := q.Depth(ctx, "fetch")

	if depthAfter != depthBefore+1 {
		t.Fatalf("depth: before=%d after=%d, want +1", depthBefore, depthAfter)
	}
	popped, err := q.BlockingPop(ctx, "fetch", 1*time.Second)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if popped != job.ID.String() {
		t.Fatalf("popped %q, want %q", popped, job.ID.String())
	}
}

func TestSweepOnce_LeavesFreshlyQueuedJobAlone(t *testing.T) {
	s, q, sw := setup(t)
	ctx := context.Background()

	_, _ = s.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	depthBefore, _ := q.Depth(ctx, "fetch")

	if err := sw.SweepOnce(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	depthAfter, _ := q.Depth(ctx, "fetch")

	if depthAfter != depthBefore {
		t.Fatalf("fresh job got re-pushed: before=%d after=%d", depthBefore, depthAfter)
	}
}
```

The `setup` helper currently builds a Sweeper with default config (no StaleThreshold). The default kicks in at 60s (set in `New`), so for the stale test we need a much shorter threshold OR backdate created_at. The test backdates — that works with the default 60s threshold.

For the "leaves fresh alone" test, the job is fresh (just enqueued). With default 60s threshold, it won't qualify as stale. Test passes.

If you want belt-and-suspenders, modify `setup` to take an optional config override and pass `StaleThreshold: 1 * time.Hour` for tests that don't care, OR backdate explicitly. Backdating is cleaner.

Run integration tests — expect undefined `sweeper.Config.StaleThreshold` (if you add it now) or expect tests to fail because `repushStale` isn't being called yet. Capture.

### Task B2: Implement

Modify `internal/sweeper/sweeper.go`:

```go
type Config struct {
	Store          *store.Store
	Queue          *queue.Queue
	Interval       time.Duration
	StaleThreshold time.Duration // default 60s
}

func New(cfg Config) *Sweeper {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.StaleThreshold == 0 {
		cfg.StaleThreshold = 60 * time.Second
	}
	return &Sweeper{cfg: cfg}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
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

func (s *Sweeper) repushStale(ctx context.Context) error {
	stale, err := s.cfg.Store.ListStaleQueuedJobs(ctx, s.cfg.StaleThreshold)
	if err != nil {
		return err
	}
	for _, j := range stale {
		if err := s.cfg.Queue.Push(ctx, j.Stage, j.ID.String()); err != nil {
			slog.Warn("repush stale failed", "job_id", j.ID, "err", err)
			continue
		}
		slog.Info("repushed stale queued job", "job_id", j.ID, "stage", j.Stage)
	}
	return nil
}
```

Run integration tests — expect 2 new sweeper tests pass + existing 4.

Commit: `feat(sweeper): repush stale queued jobs (BLPOP-to-claim race recovery)`

---

## Section C — Wire env var into cmd/sweeper

Modify `cmd/sweeper/main.go`:

Replace the `sw := sweeper.New(...)` block with:

```go
staleThresholdSec := 60
if v := os.Getenv("STALE_QUEUED_THRESHOLD_SEC"); v != "" {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		staleThresholdSec = n
	}
}

sw := sweeper.New(sweeper.Config{
	Store:          store.New(pool),
	Queue:          queue.New(redis),
	Interval:       5 * time.Second,
	StaleThreshold: time.Duration(staleThresholdSec) * time.Second,
})
```

Add `"strconv"` to imports if not present.

Append to `.env.example`:

```
# How long a 'queued' job (with no last_error) is allowed to sit before the
# sweeper re-pushes it to its Redis stage queue. Recovers from BLPOP-to-claim
# races where a worker crashed mid-claim. Default 60 seconds.
STALE_QUEUED_THRESHOLD_SEC=60
```

Build: `go build ./cmd/sweeper`. Smoke test isn't strictly necessary but harmless.

Commit: `feat(sweeper): wire STALE_QUEUED_THRESHOLD_SEC env var`

---

## Section D — Local sweep + PR + tag

```bash
make lint && make test-unit && make test-integration && make k8s-validate && make loadtest
```

All exit 0.

```bash
git push -u origin phase-3h-stale-queued

gh pr create --base main --head phase-3h-stale-queued \
  --title "fix(sweeper): Phase 3H — recover from BLPOP-to-claim race" \
  --body "Closes a real correctness gap. Before this, a worker that crashed between \`BLPOP\` (job ID gone from Redis) and \`ClaimJob\` (PG row not yet flipped to running) left the job stuck — \`status=queued\` with no \`last_error\`, invisible to the existing sweeper paths.

- store.ListStaleQueuedJobs: SELECT WHERE status=queued AND last_error IS NULL AND created_at < now() - threshold.
- store.PoolForTest exposes the pool for tests that need raw SQL (backdating created_at to simulate stale rows).
- sweeper.SweepOnce now also calls repushStale, which LPUSHes any stale-queued job IDs back onto their stage queue. Duplicate pushes are safe — ClaimJob is atomic.
- cmd/sweeper reads STALE_QUEUED_THRESHOLD_SEC env var (default 60).
- .env.example documents the new var.

3 new tests (1 store + 2 sweeper). Existing 6 store + 4 sweeper still pass."

PR=\$(gh pr view --json number -q .number)
gh pr merge \$PR --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3h -m "Phase 3H: BLPOP-to-claim race recovery via stale-queued repush"
git push origin phase-3h
git tag -fa phase-3 -m "Phase 3 milestone (extended): full Gmail->PDF->Drive pipeline incl. attachments + race recovery"
git push origin phase-3 --force
gh release create phase-3h --title "Phase 3H: BLPOP-to-claim race fix" \
  --notes "Sweeper now also re-pushes jobs that have been status='queued' with no last_error for longer than STALE_QUEUED_THRESHOLD_SEC (default 60s). Closes the one remaining correctness gap from a worker crashing mid-claim."
git push origin --delete phase-3h-stale-queued
git branch -D phase-3h-stale-queued
```

(The `phase-3` re-tag is so the milestone now points at the latest commit including 3G + 3H.)
