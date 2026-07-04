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

func (errHandler) Process(ctx context.Context, job store.Job) (string, error) {
	return "", &handlerErr{"boom"}
}

type handlerErr struct{ msg string }

func (e *handlerErr) Error() string { return e.msg }

type advancingHandler struct{ next string }

func (h advancingHandler) Process(ctx context.Context, job store.Job) (string, error) {
	return h.next, nil
}

func setupHarnessWithStage(t *testing.T, h worker.Handler, stage string) *harness {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))
	w := worker.New(worker.Config{
		Stage:             stage,
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

func TestProcessOne_AdvancesToNextStage(t *testing.T) {
	h := setupHarnessWithStage(t, advancingHandler{next: "render"}, "fetch")
	ctx := context.Background()
	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	_ = h.queue.Push(ctx, "fetch", job.ID.String())

	didWork, err := h.w.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if !didWork {
		t.Fatal("expected didWork=true")
	}

	got, _ := h.store.GetJob(ctx, job.ID)
	if got.Stage != "render" || got.Status != store.StatusQueued {
		t.Fatalf("stage/status: %s/%s, want render/queued", got.Stage, got.Status)
	}

	popped, err := h.queue.BlockingPop(ctx, "render", "w-verify", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected pushed to render: %v", err)
	}
	if popped != job.ID.String() {
		t.Fatalf("popped %q, want %q", popped, job.ID.String())
	}
}

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

func TestRun_HeartbeatsDuringSlowJob(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx, cancel := context.WithCancel(context.Background())

	job, err := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"sleepMs":2500}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.queue.Push(ctx, "test", job.ID.String()); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { _ = h.w.Run(ctx); close(done) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := h.store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never started running")
		}
		time.Sleep(50 * time.Millisecond)
	}

	time.Sleep(1500 * time.Millisecond)
	got, err := h.store.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != store.StatusRunning {
		t.Fatalf("status: %s, want running (job should still be mid-handler)", got.Status)
	}
	alive, err := h.queue.IsWorkerAlive(ctx, "worker-test-1")
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("heartbeat expired during slow job")
	}

	cancel()
	<-done
}

func TestRun_ExitsWhenSettleFails(t *testing.T) {
	h := setupHarness(t, worker.NoopHandler{})
	ctx := context.Background()

	job, err := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"sleepMs":1000}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.queue.Push(ctx, "test", job.ID.String()); err != nil {
		t.Fatal(err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- h.w.Run(ctx) }()

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := h.store.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == store.StatusRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("job never started running")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := h.store.PoolForTest().Exec(ctx, `DELETE FROM pipeline_jobs WHERE id = $1`, job.ID); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Run returned nil, want settle error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not exit after settle failure")
	}
}

func TestProcessOne_AcksOnHandlerError(t *testing.T) {
	h := setupHarness(t, errHandler{})
	ctx := context.Background()

	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = h.queue.Push(ctx, "test", job.ID.String())

	if _, err := h.w.ProcessOne(ctx); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	n, _ := h.queue.Client().LLen(ctx, "processing:test:worker-test-1").Result()
	if n != 0 {
		t.Fatalf("processing depth: %d, want 0", n)
	}
}

func TestProcessOne_AcksOnAdvance(t *testing.T) {
	h := setupHarnessWithStage(t, advancingHandler{next: "render"}, "fetch")
	ctx := context.Background()

	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	_ = h.queue.Push(ctx, "fetch", job.ID.String())

	if _, err := h.w.ProcessOne(ctx); err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	n, _ := h.queue.Client().LLen(ctx, "processing:fetch:worker-test-1").Result()
	if n != 0 {
		t.Fatalf("processing depth: %d, want 0", n)
	}
}
