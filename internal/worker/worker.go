package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Stage             string
	WorkerID          string
	Store             *store.Store
	Queue             *queue.Queue
	Handler           Handler
	PopTimeout        time.Duration
	HeartbeatTTL      time.Duration
	HeartbeatInterval time.Duration
}

type Worker struct {
	cfg Config
}

func New(cfg Config) *Worker {
	return &Worker{cfg: cfg}
}

func (w *Worker) Run(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if _, err := w.ProcessOne(ctx); err != nil {
			slog.Error("worker process failed", "err", err, "stage", w.cfg.Stage, "worker_id", w.cfg.WorkerID)
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (didWork bool, err error) {
	rawID, err := w.cfg.Queue.BlockingPop(ctx, w.cfg.Stage, w.cfg.PopTimeout)
	if errors.Is(err, queue.ErrEmpty) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pop: %w", err)
	}

	jobID, err := uuid.Parse(rawID)
	if err != nil {
		return true, fmt.Errorf("parse job id %q: %w", rawID, err)
	}

	if err := w.cfg.Store.ClaimJob(ctx, jobID, w.cfg.WorkerID); err != nil {
		if errors.Is(err, store.ErrJobNotClaimable) {
			slog.Info("claim race lost", "job_id", jobID, "worker_id", w.cfg.WorkerID)
			return true, nil
		}
		return true, fmt.Errorf("claim: %w", err)
	}

	job, err := w.cfg.Store.GetJob(ctx, jobID)
	if err != nil {
		return true, fmt.Errorf("get: %w", err)
	}

	hbCtx, hbCancel := context.WithCancel(ctx)
	go w.heartbeatLoop(hbCtx)

	handlerErr := w.cfg.Handler.Process(ctx, job)

	hbCancel()

	if handlerErr != nil {
		nextRun := time.Now().Add(Compute(job.Attempts + 1))
		if err := w.cfg.Store.MarkFailed(ctx, jobID, handlerErr.Error(), nextRun); err != nil {
			return true, fmt.Errorf("mark failed: %w", err)
		}
		slog.Info("job failed, will retry", "job_id", jobID, "attempts", job.Attempts+1, "next_run", nextRun)
		return true, nil
	}

	if err := w.cfg.Store.MarkDone(ctx, jobID); err != nil {
		return true, fmt.Errorf("mark done: %w", err)
	}
	return true, nil
}

func (w *Worker) heartbeatLoop(ctx context.Context) {
	tick := time.NewTicker(w.cfg.HeartbeatInterval)
	defer tick.Stop()

	if err := w.cfg.Queue.Heartbeat(ctx, w.cfg.WorkerID, w.cfg.HeartbeatTTL); err != nil {
		slog.Warn("heartbeat write failed", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := w.cfg.Queue.Heartbeat(ctx, w.cfg.WorkerID, w.cfg.HeartbeatTTL); err != nil {
				slog.Warn("heartbeat write failed", "err", err)
			}
		}
	}
}
