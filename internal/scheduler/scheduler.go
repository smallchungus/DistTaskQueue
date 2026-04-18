package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
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
}

type Scheduler struct {
	cfg Config
}

func New(cfg Config) *Scheduler {
	if cfg.Interval == 0 {
		cfg.Interval = 5 * time.Minute
	}
	return &Scheduler{cfg: cfg}
}

func (s *Scheduler) Run(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Interval)
	defer tick.Stop()

	if err := s.PollOnce(ctx); err != nil {
		slog.Error("initial poll", "err", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if err := s.PollOnce(ctx); err != nil {
				slog.Error("poll", "err", err)
			}
		}
	}
}

func (s *Scheduler) PollOnce(ctx context.Context) error {
	users, err := s.cfg.Store.ListUsers(ctx)
	if err != nil {
		return fmt.Errorf("list users: %w", err)
	}
	for _, u := range users {
		if err := s.pollUser(ctx, u); err != nil {
			slog.Warn("poll user failed", "user_id", u.ID, "err", err)
		}
	}
	return nil
}

func (s *Scheduler) pollUser(ctx context.Context, u store.User) error {
	cursor, err := s.cfg.Store.GetGmailSyncState(ctx, u.ID)
	if err != nil {
		return err
	}

	if cursor == "" {
		hid, err := s.fetchInitialHistoryID(ctx, u.ID)
		if err != nil {
			return fmt.Errorf("init history id: %w", err)
		}
		if err := s.cfg.Store.SetGmailSyncState(ctx, u.ID, hid); err != nil {
			return fmt.Errorf("save init cursor: %w", err)
		}
		slog.Info("initialized sync cursor", "user_id", u.ID, "history_id", hid)
		return nil
	}

	client, err := gmail.New(ctx, gmail.Config{
		Store:         s.cfg.Store,
		UserID:        u.ID,
		EncryptionKey: s.cfg.EncryptionKey,
		OAuth2:        s.cfg.OAuth2,
		Endpoint:      s.cfg.GmailEndpoint,
	})
	if err != nil {
		return fmt.Errorf("gmail client: %w", err)
	}

	ids, newCursor, err := client.LatestMessageIDs(ctx, cursor)
	if err != nil {
		return fmt.Errorf("list messages: %w", err)
	}

	for _, msgID := range ids {
		gid := msgID
		uid := u.ID
		job, err := s.cfg.Store.EnqueueJob(ctx, store.NewJob{
			Stage:          "fetch",
			UserID:         &uid,
			GmailMessageID: &gid,
			Payload:        json.RawMessage(`{}`),
		})
		if err != nil {
			slog.Warn("enqueue failed", "msg_id", gid, "err", err)
			continue
		}
		if err := s.cfg.Queue.Push(ctx, "fetch", job.ID.String()); err != nil {
			slog.Warn("push failed", "job_id", job.ID, "err", err)
			continue
		}
		slog.Info("enqueued fetch", "job_id", job.ID, "msg_id", gid)
	}

	if newCursor != cursor {
		if err := s.cfg.Store.SetGmailSyncState(ctx, u.ID, newCursor); err != nil {
			return fmt.Errorf("save cursor: %w", err)
		}
	}
	return nil
}

func (s *Scheduler) fetchInitialHistoryID(ctx context.Context, userID uuid.UUID) (string, error) {
	tok, err := oauth.LoadToken(ctx, s.cfg.Store, userID, s.cfg.EncryptionKey, "google")
	if err != nil {
		return "", err
	}
	base := s.cfg.OAuth2.TokenSource(ctx, tok)
	saving := oauth.NewSavingSource(base, func(t *oauth2.Token) error {
		return oauth.SaveToken(ctx, s.cfg.Store, userID, s.cfg.EncryptionKey, "google", t)
	}, tok)
	httpClient := oauth2.NewClient(ctx, saving)

	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if s.cfg.GmailEndpoint != "" {
		opts = append(opts, option.WithEndpoint(s.cfg.GmailEndpoint))
	}
	svc, err := gmailapi.NewService(ctx, opts...)
	if err != nil {
		return "", fmt.Errorf("gmail svc: %w", err)
	}
	prof, err := svc.Users.GetProfile("me").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("get profile: %w", err)
	}
	return fmt.Sprintf("%d", prof.HistoryId), nil
}
