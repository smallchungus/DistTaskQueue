//go:build loadtest

package loadtest_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
	"github.com/smallchungus/disttaskqueue/internal/worker"
)

func TestLoad_5000Jobs_4Workers_Under15s(t *testing.T) {
	const (
		jobCount    = 5000
		workerCount = 4
		stage       = "loadtest"
		deadline    = 15 * time.Second
	)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	t.Logf("enqueueing %d jobs", jobCount)
	enqueueStart := time.Now()
	jobIDs := make([]string, 0, jobCount)
	for i := 0; i < jobCount; i++ {
		j, err := s.EnqueueJob(context.Background(), store.NewJob{Stage: stage})
		if err != nil {
			t.Fatalf("enqueue %d: %v", i, err)
		}
		jobIDs = append(jobIDs, j.ID.String())
	}
	for _, id := range jobIDs {
		if err := q.Push(context.Background(), stage, id); err != nil {
			t.Fatalf("push %s: %v", id, err)
		}
	}
	t.Logf("enqueue complete in %v", time.Since(enqueueStart))

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var wg sync.WaitGroup
	processStart := time.Now()
	for i := 0; i < workerCount; i++ {
		wg.Add(1)
		w := worker.New(worker.Config{
			Stage:             stage,
			WorkerID:          fmt.Sprintf("loadtest-w%d", i),
			Store:             s,
			Queue:             q,
			Handler:           worker.NoopHandler{},
			PopTimeout:        1 * time.Second,
			HeartbeatTTL:      1 * time.Second,
			HeartbeatInterval: 200 * time.Millisecond,
		})
		go func() {
			defer wg.Done()
			_ = w.Run(runCtx)
		}()
	}

	pollDeadline := time.Now().Add(deadline)
	for time.Now().Before(pollDeadline) {
		var done int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pipeline_jobs WHERE stage = $1 AND status = 'done'`,
			stage,
		).Scan(&done)
		if err != nil {
			t.Fatalf("poll: %v", err)
		}
		if done >= jobCount {
			elapsed := time.Since(processStart)
			t.Logf("ALL DONE: %d jobs / %d workers in %v (%.0f jobs/s)",
				jobCount, workerCount, elapsed, float64(jobCount)/elapsed.Seconds())
			cancel()
			wg.Wait()
			if elapsed > deadline {
				t.Fatalf("too slow: %v > %v", elapsed, deadline)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}

	var done int
	_ = pool.QueryRow(context.Background(),
		`SELECT count(*) FROM pipeline_jobs WHERE stage = $1 AND status = 'done'`, stage).Scan(&done)
	cancel()
	wg.Wait()
	t.Fatalf("deadline exceeded: only %d / %d jobs done after %v", done, jobCount, deadline)
}
