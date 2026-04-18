package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/sweeper"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	dsn := envOr("DATABASE_URL", "postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")

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

	staleThresholdSec := 60
	if v := os.Getenv("STALE_QUEUED_THRESHOLD_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			staleThresholdSec = n
		}
	}

	sw := sweeper.New(sweeper.Config{
		Store:          store.New(pool),
		Queue:          queue.New(redis),
		Interval:       5 * time.Second,
		StaleThreshold: time.Duration(staleThresholdSec) * time.Second,
	})

	slog.Info("sweeper starting")
	if err := sw.Run(ctx); err != nil {
		slog.Error("sweeper run", "err", err)
		os.Exit(1)
	}
	slog.Info("sweeper stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
