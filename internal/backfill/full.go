package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type FullConfig struct {
	Store         *store.Store
	Queue         *queue.Queue
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	GmailEndpoint string
	Email         string
	Since         time.Time
	Before        time.Time
	MaxQueueDepth int64
	PollInterval  time.Duration
}

// RunFull imports a user's existing Gmail archive: pages messages.list for
// the given date range and enqueues fetch jobs with the same idempotency
// check the scheduler uses, so an interrupted run can be safely re-invoked.
// It pauses before each page while queue:fetch is deeper than MaxQueueDepth.
func RunFull(ctx context.Context, cfg FullConfig) (enqueued, skipped int, err error) {
	if cfg.MaxQueueDepth <= 0 {
		cfg.MaxQueueDepth = 500
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 5 * time.Second
	}

	u, err := cfg.Store.GetUserByEmail(ctx, cfg.Email)
	if err != nil {
		return 0, 0, fmt.Errorf("get user: %w", err)
	}

	client, err := gmail.New(ctx, gmail.Config{
		Store:         cfg.Store,
		UserID:        u.ID,
		EncryptionKey: cfg.EncryptionKey,
		OAuth2:        cfg.OAuth2,
		Endpoint:      cfg.GmailEndpoint,
	})
	if err != nil {
		return 0, 0, fmt.Errorf("gmail client: %w", err)
	}

	page := 0
	err = client.ListAllPages(ctx, cfg.Since, cfg.Before, func(ids []string) error {
		page++
		if err := waitForQueueDepth(ctx, cfg, page, enqueued, skipped); err != nil {
			return err
		}
		for _, msgID := range ids {
			enq, err := enqueueOne(ctx, cfg, u.ID, msgID)
			if err != nil {
				return err
			}
			if enq {
				enqueued++
			} else {
				skipped++
			}
		}
		slog.Info("backfill page done", "page", page, "enqueued", enqueued, "skipped", skipped)
		return nil
	})
	if err != nil {
		return enqueued, skipped, err
	}
	return enqueued, skipped, nil
}

func waitForQueueDepth(ctx context.Context, cfg FullConfig, page, enqueued, skipped int) error {
	for {
		depth, err := cfg.Queue.Depth(ctx, "fetch")
		if err != nil {
			return fmt.Errorf("queue depth: %w", err)
		}
		if depth <= cfg.MaxQueueDepth {
			return nil
		}
		slog.Info("backfill waiting on queue depth", "page", page, "enqueued", enqueued, "skipped", skipped, "depth", depth)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(cfg.PollInterval):
		}
	}
}

func enqueueOne(ctx context.Context, cfg FullConfig, userID uuid.UUID, msgID string) (bool, error) {
	has, err := cfg.Store.HasJobForMessage(ctx, userID, msgID)
	if err != nil {
		return false, fmt.Errorf("has job for message %s: %w", msgID, err)
	}
	if has {
		return false, nil
	}
	gid := msgID
	uid := userID
	job, err := cfg.Store.EnqueueJob(ctx, store.NewJob{
		Stage:          "fetch",
		UserID:         &uid,
		GmailMessageID: &gid,
		Payload:        json.RawMessage(`{}`),
	})
	if err != nil {
		return false, fmt.Errorf("enqueue %s: %w", msgID, err)
	}
	if err := cfg.Queue.Push(ctx, "fetch", job.ID.String()); err != nil {
		return false, fmt.Errorf("push %s: %w", msgID, err)
	}
	return true, nil
}
