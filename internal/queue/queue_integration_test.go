//go:build integration

package queue_test

import (
	"context"
	"testing"
	"time"

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

func TestBlockingPop_ReturnsPushedJobID(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	if err := q.Push(ctx, "fetch", "job-abc"); err != nil {
		t.Fatal(err)
	}

	jobID, err := q.BlockingPop(ctx, "fetch", 2*time.Second)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if jobID != "job-abc" {
		t.Fatalf("jobID: got %q, want %q", jobID, "job-abc")
	}
}

func TestBlockingPop_FIFOOrdering(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := q.Push(ctx, "fetch", id); err != nil {
			t.Fatal(err)
		}
	}

	for _, want := range []string{"a", "b", "c"} {
		got, err := q.BlockingPop(ctx, "fetch", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	}
}

func TestBlockingPop_ReturnsErrEmptyOnTimeout(t *testing.T) {
	q := newQueue(t)
	_, err := q.BlockingPop(context.Background(), "fetch", 200*time.Millisecond)
	if err != queue.ErrEmpty {
		t.Fatalf("got %v, want ErrEmpty", err)
	}
}

func TestIsWorkerAlive_TrueWhenHeartbeatSet(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()
	if err := q.Heartbeat(ctx, "w1", 5*time.Second); err != nil {
		t.Fatal(err)
	}
	alive, err := q.IsWorkerAlive(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if !alive {
		t.Fatal("expected alive=true")
	}
}

func TestIsWorkerAlive_FalseWhenHeartbeatMissing(t *testing.T) {
	q := newQueue(t)
	alive, err := q.IsWorkerAlive(context.Background(), "never-lived")
	if err != nil {
		t.Fatal(err)
	}
	if alive {
		t.Fatal("expected alive=false")
	}
}

func TestHeartbeat_SetsKeyWithTTL(t *testing.T) {
	q := newQueue(t)
	cli := q.Client()

	if err := q.Heartbeat(context.Background(), "worker-xyz", 5*time.Second); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}

	val, err := cli.Get(context.Background(), "heartbeat:worker-xyz").Result()
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if val == "" {
		t.Fatalf("empty value")
	}
	ttl, err := cli.TTL(context.Background(), "heartbeat:worker-xyz").Result()
	if err != nil {
		t.Fatalf("ttl: %v", err)
	}
	if ttl <= 0 || ttl > 5*time.Second {
		t.Fatalf("ttl out of range: %v", ttl)
	}
}
