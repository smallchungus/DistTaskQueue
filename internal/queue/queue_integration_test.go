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
