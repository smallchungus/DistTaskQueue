//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return store.New(pool)
}

func TestEnqueueJob_PersistsRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	j, err := s.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"k":"v"}`)})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if j.ID.String() == "" || j.Stage != "test" || j.Status != store.StatusQueued {
		t.Fatalf("unexpected job: %+v", j)
	}
	if string(j.Payload) != `{"k": "v"}` && string(j.Payload) != `{"k":"v"}` {
		t.Fatalf("payload roundtrip: got %s", string(j.Payload))
	}
}

func TestGetJob_RoundTripsAllFields(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	enq, err := s.EnqueueJob(ctx, store.NewJob{Stage: "test", Payload: json.RawMessage(`{"x":1}`)})
	if err != nil {
		t.Fatal(err)
	}

	got, err := s.GetJob(ctx, enq.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != enq.ID || got.Stage != enq.Stage || got.Status != enq.Status {
		t.Fatalf("mismatch: enq=%+v got=%+v", enq, got)
	}
}

func TestGetJob_ReturnsErrNotFoundForUnknownID(t *testing.T) {
	s := newStore(t)
	_, err := s.GetJob(context.Background(), uuid.New())
	if !errors.Is(err, store.ErrJobNotFound) {
		t.Fatalf("got %v, want ErrJobNotFound", err)
	}
}

func TestClaimJob_TransitionsQueuedToRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})

	if err := s.ClaimJob(ctx, j.ID, "worker-1"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusRunning {
		t.Fatalf("status: %s", got.Status)
	}
	if got.WorkerID == nil || *got.WorkerID != "worker-1" {
		t.Fatalf("worker_id: %v", got.WorkerID)
	}
	if got.ClaimedAt == nil {
		t.Fatalf("claimed_at: nil")
	}
}

func TestClaimJob_FailsWhenNotQueued(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	if err := s.ClaimJob(ctx, j.ID, "worker-1"); err != nil {
		t.Fatal(err)
	}

	err := s.ClaimJob(ctx, j.ID, "worker-2")
	if !errors.Is(err, store.ErrJobNotClaimable) {
		t.Fatalf("got %v, want ErrJobNotClaimable", err)
	}
}

func TestMarkDone_SetsTerminalState(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j.ID, "worker-1")

	if err := s.MarkDone(ctx, j.ID); err != nil {
		t.Fatalf("done: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusDone {
		t.Fatalf("status: %s", got.Status)
	}
	if got.CompletedAt == nil {
		t.Fatalf("completed_at: nil")
	}
}

func TestMarkFailed_RequeuesWithIncrementedAttempts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j.ID, "worker-1")

	nextRun := time.Now().Add(2 * time.Second)
	if err := s.MarkFailed(ctx, j.ID, "transient boom", nextRun); err != nil {
		t.Fatalf("fail: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusQueued {
		t.Fatalf("status: %s, want queued", got.Status)
	}
	if got.Attempts != 1 {
		t.Fatalf("attempts: %d, want 1", got.Attempts)
	}
	if got.LastError == nil || *got.LastError != "transient boom" {
		t.Fatalf("last_error: %v", got.LastError)
	}
	if got.WorkerID != nil {
		t.Fatalf("worker_id: %v, want nil after fail", got.WorkerID)
	}
}

func TestListRunningJobs_ReturnsOnlyRunning(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	jQueued, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	jRunning, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, jRunning.ID, "w1")
	jDone, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, jDone.ID, "w2")
	_ = s.MarkDone(ctx, jDone.ID)

	jobs, err := s.ListRunningJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d jobs, want 1", len(jobs))
	}
	if jobs[0].ID != jRunning.ID {
		t.Fatalf("got id %s, want %s", jobs[0].ID, jRunning.ID)
	}
	_ = jQueued
}

func TestListReadyRetryJobs_ReturnsQueuedWithLastErrorAndDueNextRun(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	j1, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j1.ID, "w1")
	past := time.Now().Add(-1 * time.Minute)
	_ = s.MarkFailed(ctx, j1.ID, "boom", past)

	j2, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})
	_ = s.ClaimJob(ctx, j2.ID, "w1")
	future := time.Now().Add(5 * time.Minute)
	_ = s.MarkFailed(ctx, j2.ID, "boom", future)

	j3, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})

	jobs, err := s.ListReadyRetryJobs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 {
		t.Fatalf("got %d, want 1; got IDs: %v", len(jobs), jobIDs(jobs))
	}
	if jobs[0].ID != j1.ID {
		t.Fatalf("got %s, want %s", jobs[0].ID, j1.ID)
	}
	_, _ = j2, j3
}

func TestCreateUser_PersistsRow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	u, err := s.CreateUser(ctx, "test@example.com")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if u.ID.String() == "" || u.Email != "test@example.com" {
		t.Fatalf("user: %+v", u)
	}

	got, err := s.GetUserByEmail(ctx, "test@example.com")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.ID != u.ID {
		t.Fatalf("id mismatch: %s vs %s", got.ID, u.ID)
	}
}

func TestGetUserByEmail_ReturnsErrUserNotFound(t *testing.T) {
	s := newStore(t)
	_, err := s.GetUserByEmail(context.Background(), "nobody@example.com")
	if !errors.Is(err, store.ErrUserNotFound) {
		t.Fatalf("got %v, want ErrUserNotFound", err)
	}
}

func jobIDs(js []store.Job) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID.String()
	}
	return out
}

func TestMarkFailed_MarksDeadAtMaxAttempts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "test"})

	for i := 0; i < 8; i++ {
		_ = s.ClaimJob(ctx, j.ID, "worker-1")
		_ = s.MarkFailed(ctx, j.ID, "still broken", time.Now())
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Status != store.StatusDead {
		t.Fatalf("status: %s, want dead", got.Status)
	}
}
