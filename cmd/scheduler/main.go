package main

import (
	"context"
	"encoding/base64"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

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

	sch := scheduler.New(scheduler.Config{
		Store:         store.New(pool),
		Queue:         queue.New(redis),
		EncryptionKey: key,
		OAuth2: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			Endpoint:     google.Endpoint,
			Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
		},
		Interval: 5 * time.Minute,
	})

	slog.Info("scheduler starting")
	if err := sch.Run(ctx); err != nil {
		slog.Error("scheduler run", "err", err)
		os.Exit(1)
	}
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
