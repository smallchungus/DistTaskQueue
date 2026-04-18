//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"testing"

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
