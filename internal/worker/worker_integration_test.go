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
