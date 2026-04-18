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
	_, err := h.Process(context.Background(), job)
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
	if _, err := h.Process(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 50*time.Millisecond {
		t.Fatalf("should be instant")
	}
}
