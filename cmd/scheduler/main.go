package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/smallchungus/disttaskqueue/internal/backfill"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/scheduler"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

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
	s := store.New(pool)
	q := queue.New(redis)

	sch := scheduler.New(scheduler.Config{
		Store:         s,
		Queue:         q,
		EncryptionKey: key,
		OAuth2:        oauthCfg,
		Interval:      time.Duration(envIntOr("SCHEDULER_POLL_INTERVAL_SEC", 60)) * time.Second,
	})
	bf := backfill.New(backfill.Config{
		Store:         s,
		Queue:         q,
		EncryptionKey: key,
		OAuth2:        oauthCfg,
		Interval:      time.Duration(envIntOr("BACKFILL_INTERVAL_SEC", 3600)) * time.Second,
		Window:        time.Duration(envIntOr("BACKFILL_WINDOW_HOURS", 24)) * time.Hour,
	})

	slog.Info("scheduler starting")
	errCh := make(chan error, 2)
	go func() { errCh <- sch.Run(ctx) }()
	go func() { errCh <- bf.Run(ctx) }()
	// Wait for either goroutine to return. scheduler.Run and backfill.Run both
	// exit cleanly on ctx cancellation, so the first error here is the real one.
	if err := <-errCh; err != nil {
		slog.Error("scheduler run", "err", err)
		os.Exit(1)
	}
	<-errCh
	slog.Info("scheduler stopped")
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

// envIntOr reads an env var as an integer. Non-positive / invalid values fall
// back to def. Required so the poll interval can be tuned without recompiling.
func envIntOr(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}
