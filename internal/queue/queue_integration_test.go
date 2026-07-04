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

	jobID, err := q.BlockingPop(ctx, "fetch", "w1", 2*time.Second)
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
		got, err := q.BlockingPop(ctx, "fetch", "w1", time.Second)
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
	_, err := q.BlockingPop(context.Background(), "fetch", "w1", 200*time.Millisecond)
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

func TestBlockingPop_MovesJobToProcessingList(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	if err := q.Push(ctx, "fetch", "job-1"); err != nil {
		t.Fatal(err)
	}
	got, err := q.BlockingPop(ctx, "fetch", "w1", time.Second)
	if err != nil {
		t.Fatalf("pop: %v", err)
	}
	if got != "job-1" {
		t.Fatalf("got %q, want %q", got, "job-1")
	}

	entries, err := q.Client().LRange(ctx, "processing:fetch:w1", 0, -1).Result()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0] != "job-1" {
		t.Fatalf("processing list: %v, want [job-1]", entries)
	}
}

func TestAck_RemovesJobFromProcessingList(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	_ = q.Push(ctx, "fetch", "job-1")
	if _, err := q.BlockingPop(ctx, "fetch", "w1", time.Second); err != nil {
		t.Fatal(err)
	}

	if err := q.Ack(ctx, "fetch", "w1", "job-1"); err != nil {
		t.Fatalf("ack: %v", err)
	}
	n, err := q.Client().LLen(ctx, "processing:fetch:w1").Result()
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("processing depth: %d, want 0", n)
	}
}

func TestListProcessing_ReturnsStageAndWorker(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	_ = q.Push(ctx, "fetch", "job-1")
	if _, err := q.BlockingPop(ctx, "fetch", "w1", time.Second); err != nil {
		t.Fatal(err)
	}

	refs, err := q.ListProcessing(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].Stage != "fetch" || refs[0].WorkerID != "w1" {
		t.Fatalf("refs: %+v, want [{fetch w1}]", refs)
	}
}

func TestReclaimProcessing_MovesJobsBackToQueue(t *testing.T) {
	q := newQueue(t)
	ctx := context.Background()

	_ = q.Push(ctx, "fetch", "job-1")
	if _, err := q.BlockingPop(ctx, "fetch", "w1", time.Second); err != nil {
		t.Fatal(err)
	}

	n, err := q.ReclaimProcessing(ctx, "fetch", "w1")
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Fatalf("reclaimed: %d, want 1", n)
	}
	depth, _ := q.Depth(ctx, "fetch")
	if depth != 1 {
		t.Fatalf("queue depth: %d, want 1", depth)
	}
	left, _ := q.Client().LLen(ctx, "processing:fetch:w1").Result()
	if left != 0 {
		t.Fatalf("processing depth: %d, want 0", left)
	}
}
