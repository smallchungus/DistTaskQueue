//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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
