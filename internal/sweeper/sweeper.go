package sweeper

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Store          *store.Store
	Queue          *queue.Queue
	Interval       time.Duration
	StaleThreshold time.Duration
}

type Sweeper struct {
	cfg Config
}

func New(cfg Config) *Sweeper {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Second
	}
	if cfg.StaleThreshold == 0 {
		cfg.StaleThreshold = 60 * time.Second
	}
	return &Sweeper{cfg: cfg}
}

func (s *Sweeper) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := s.SweepOnce(ctx); err != nil {
				slog.Error("sweep failed", "err", err)
			}
		}
	}
}

func (s *Sweeper) SweepOnce(ctx context.Context) error {
	if err := s.reviveOrphans(ctx); err != nil {
		return fmt.Errorf("revive orphans: %w", err)
	}
	if err := s.promoteDelayed(ctx); err != nil {
		return fmt.Errorf("promote delayed: %w", err)
	}
	if err := s.repushStale(ctx); err != nil {
		return fmt.Errorf("repush stale: %w", err)
	}
	return nil
}

func (s *Sweeper) reviveOrphans(ctx context.Context) error {
	running, err := s.cfg.Store.ListRunningJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range running {
		if j.WorkerID == nil {
			continue
		}
		alive, err := s.cfg.Queue.IsWorkerAlive(ctx, *j.WorkerID)
		if err != nil {
			slog.Warn("heartbeat check failed", "job_id", j.ID, "err", err)
			continue
		}
		if alive {
			continue
		}
		if err := s.cfg.Store.MarkFailed(ctx, j.ID, "worker died", time.Now()); err != nil {
			slog.Warn("revive failed", "job_id", j.ID, "err", err)
			continue
		}
		slog.Info("revived orphan", "job_id", j.ID, "worker_id", *j.WorkerID)
	}
	return nil
}

func (s *Sweeper) repushStale(ctx context.Context) error {
	stale, err := s.cfg.Store.ListStaleQueuedJobs(ctx, s.cfg.StaleThreshold)
	if err != nil {
		return err
	}
	for _, j := range stale {
		if err := s.cfg.Queue.Push(ctx, j.Stage, j.ID.String()); err != nil {
			slog.Warn("repush stale failed", "job_id", j.ID, "err", err)
			continue
		}
		slog.Info("repushed stale queued job", "job_id", j.ID, "stage", j.Stage)
	}
	return nil
}

func (s *Sweeper) promoteDelayed(ctx context.Context) error {
	ready, err := s.cfg.Store.ListReadyRetryJobs(ctx)
	if err != nil {
		return err
	}
	for _, j := range ready {
		if err := s.cfg.Queue.Push(ctx, j.Stage, j.ID.String()); err != nil {
			slog.Warn("promote failed", "job_id", j.ID, "err", err)
			continue
		}
		slog.Info("promoted retry", "job_id", j.ID, "stage", j.Stage)
	}
	return nil
}
