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

	if err := q.Heartbeat(ctx, "w1", 500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	job, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	if err := s.ClaimJob(ctx, job.ID, "w1"); err != nil {
		t.Fatal(err)
	}
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
