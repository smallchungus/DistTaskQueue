package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Store         *store.Store
	Queue         *queue.Queue
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	GmailEndpoint string
	Interval      time.Duration
	Window        time.Duration
}

type Backfill struct{ cfg Config }

func New(cfg Config) *Backfill {
	if cfg.Window == 0 {
		cfg.Window = 24 * time.Hour
	}
	return &Backfill{cfg: cfg}
}

// Run loops RunOnce on Interval until ctx cancels. Interval==0 disables
// backfill: Run returns immediately so the scheduler binary can keep its
// primary poll loop going without a second goroutine.
func (b *Backfill) Run(ctx context.Context) error {
	if b.cfg.Interval <= 0 {
		slog.Info("backfill disabled")
		return nil
	}
	tick := time.NewTicker(b.cfg.Interval)
	defer tick.Stop()

	if err := b.RunOnce(ctx); err != nil {
		slog.Error("backfill initial", "err", err)
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := b.RunOnce(ctx); err != nil {
				slog.Error("backfill", "err", err)
			}
		}
	}
}

// RunOnce queries messages.list for each user, enqueues fetch jobs for any
// message ID not already tracked in pipeline_jobs. Idempotent — safe to call
// on every tick even if no new messages arrived.
func (b *Backfill) RunOnce(ctx context.Context) error {
	users, err := b.cfg.Store.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if err := b.runUser(ctx, u); err != nil {
			slog.Warn("backfill user failed", "user_id", u.ID, "err", err)
		}
	}
	return nil
}

func (b *Backfill) runUser(ctx context.Context, u store.User) error {
	client, err := gmail.New(ctx, gmail.Config{
		Store:         b.cfg.Store,
		UserID:        u.ID,
		EncryptionKey: b.cfg.EncryptionKey,
		OAuth2:        b.cfg.OAuth2,
		Endpoint:      b.cfg.GmailEndpoint,
	})
	if err != nil {
		return fmt.Errorf("gmail client: %w", err)
	}

	since := time.Now().Add(-b.cfg.Window)
	ids, err := client.ListRecent(ctx, since)
	if err != nil {
		return fmt.Errorf("list recent: %w", err)
	}

	enqueued, skipped := 0, 0
	for _, msgID := range ids {
		has, err := b.cfg.Store.HasJobForMessage(ctx, u.ID, msgID)
		if err != nil {
			slog.Warn("backfill has-job check", "msg_id", msgID, "err", err)
			continue
		}
		if has {
			skipped++
			continue
		}
		gid := msgID
		uid := u.ID
		job, err := b.cfg.Store.EnqueueJob(ctx, store.NewJob{
			Stage:          "fetch",
			UserID:         &uid,
			GmailMessageID: &gid,
			Payload:        json.RawMessage(`{}`),
		})
		if err != nil {
			slog.Warn("backfill enqueue failed", "msg_id", msgID, "err", err)
			continue
		}
		if err := b.cfg.Queue.Push(ctx, "fetch", job.ID.String()); err != nil {
			slog.Warn("backfill push failed", "job_id", job.ID, "err", err)
			continue
		}
		enqueued++
	}
	slog.Info("backfill user done", "user_id", u.ID, "candidates", len(ids), "enqueued", enqueued, "skipped", skipped)
	return nil
}
