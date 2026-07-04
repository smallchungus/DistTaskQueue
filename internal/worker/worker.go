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

var errPostClaim = errors.New("post-claim settle failed")

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
	if err := w.cfg.Queue.Heartbeat(ctx, w.cfg.WorkerID, w.cfg.HeartbeatTTL); err != nil {
		slog.Warn("heartbeat write failed", "err", err)
	}
	hbCtx, hbCancel := context.WithCancel(ctx)
	defer hbCancel()
	go w.heartbeatLoop(hbCtx)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if _, err := w.ProcessOne(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, errPostClaim) {
				return err
			}
			slog.Error("worker process failed", "err", err, "stage", w.cfg.Stage, "worker_id", w.cfg.WorkerID)
		}
	}
}

func (w *Worker) ProcessOne(ctx context.Context) (didWork bool, err error) {
	rawID, err := w.cfg.Queue.BlockingPop(ctx, w.cfg.Stage, w.cfg.WorkerID, w.cfg.PopTimeout)
	if errors.Is(err, queue.ErrEmpty) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("pop: %w", err)
	}

	defer func() {
		if ackErr := w.cfg.Queue.Ack(context.WithoutCancel(ctx), w.cfg.Stage, w.cfg.WorkerID, rawID); ackErr != nil {
			slog.Warn("ack failed", "job_id", rawID, "err", ackErr)
		}
	}()

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
		return true, fmt.Errorf("get: %w: %w", err, errPostClaim)
	}

	nextStage, handlerErr := w.cfg.Handler.Process(ctx, job)

	if handlerErr != nil {
		nextRun := time.Now().Add(Compute(job.Attempts + 1))
		if err := w.cfg.Store.MarkFailed(ctx, jobID, handlerErr.Error(), nextRun); err != nil {
			return true, fmt.Errorf("mark failed: %w: %w", err, errPostClaim)
		}
		slog.Info("job failed, will retry", "job_id", jobID, "attempts", job.Attempts+1, "next_run", nextRun)
		return true, nil
	}

	if nextStage != "" {
		if err := w.cfg.Store.AdvanceJob(ctx, jobID, nextStage); err != nil {
			return true, fmt.Errorf("advance: %w: %w", err, errPostClaim)
		}
		if err := w.cfg.Queue.Push(ctx, nextStage, jobID.String()); err != nil {
			return true, fmt.Errorf("push next stage: %w", err)
		}
		slog.Info("job advanced", "job_id", jobID, "from", job.Stage, "to", nextStage)
		return true, nil
	}

	if err := w.cfg.Store.MarkDone(ctx, jobID); err != nil {
		return true, fmt.Errorf("mark done: %w: %w", err, errPostClaim)
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
