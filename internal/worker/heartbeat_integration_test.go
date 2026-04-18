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
