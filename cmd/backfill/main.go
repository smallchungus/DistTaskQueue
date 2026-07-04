// Package main is a one-shot operator command that imports a user's
// existing Gmail archive into the pipeline. The scheduler stays
// forward-sync-only; this covers the one-time backfill.
//
// Usage:
//
//	./backfill --email=alice@example.com --since=2020-01-01
//
// Environment variables required:
//
//	DATABASE_URL, REDIS_URL             defaults match the compose stack
//	GOOGLE_OAUTH_CLIENT_ID/SECRET       from Google Cloud OAuth 2.0 Client IDs page
//	TOKEN_ENCRYPTION_KEY                base64 of 32 random bytes (also used by workers)
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/smallchungus/disttaskqueue/internal/backfill"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

const dateFormat = "2006-01-02"

func main() {
	email := flag.String("email", "", "email address of the account to backfill (required)")
	since := flag.String("since", "", "only messages after this date, YYYY-MM-DD (optional)")
	before := flag.String("before", "", "only messages before this date, YYYY-MM-DD (optional)")
	maxQueue := flag.Int64("max-queue", 500, "pause enqueueing while queue:fetch depth exceeds this")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *email == "" {
		slog.Error("--email is required")
		os.Exit(2)
	}
	sinceTime, err := parseDateFlag(*since)
	if err != nil {
		slog.Error("--since", "err", err)
		os.Exit(2)
	}
	beforeTime, err := parseDateFlag(*before)
	if err != nil {
		slog.Error("--before", "err", err)
		os.Exit(2)
	}

	dsn := envOr("DATABASE_URL", "postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")
	clientID := mustEnv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := mustEnv("GOOGLE_OAUTH_CLIENT_SECRET")
	keyB64 := mustEnv("TOKEN_ENCRYPTION_KEY")

	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		slog.Error("TOKEN_ENCRYPTION_KEY must decode to 32 bytes", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, dsn); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		slog.Error("pg connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()

	opts, err := goredis.ParseURL(redisURL)
	if err != nil {
		slog.Error("redis url", "err", err)
		os.Exit(1)
	}
	redis := goredis.NewClient(opts)
	defer func() { _ = redis.Close() }()

	oauthCfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
	}

	slog.Info("backfill starting", "email", *email, "since", *since, "before", *before, "max_queue", *maxQueue)
	enqueued, skipped, err := backfill.RunFull(ctx, backfill.FullConfig{
		Store:         store.New(pool),
		Queue:         queue.New(redis),
		EncryptionKey: key,
		OAuth2:        oauthCfg,
		Email:         *email,
		Since:         sinceTime,
		Before:        beforeTime,
		MaxQueueDepth: *maxQueue,
	})
	if err != nil {
		slog.Error("backfill run", "err", err)
		os.Exit(1)
	}
	slog.Info("backfill done", "enqueued", enqueued, "skipped", skipped)
	fmt.Printf("enqueued=%d skipped=%d\n", enqueued, skipped)
}

func parseDateFlag(v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(dateFormat, v)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing env var", "key", k)
		os.Exit(1)
	}
	return v
}
