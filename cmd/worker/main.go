package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/worker"
)

func main() {
	stage := flag.String("stage", "", "queue stage name (required)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *stage == "" {
		slog.Error("--stage is required")
		os.Exit(2)
	}

	dsn := envOr("DATABASE_URL", "postgres://dtq:dtq@localhost:5432/dtq?sslmode=disable")
	redisURL := envOr("REDIS_URL", "redis://localhost:6379/0")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := store.Migrate(ctx, dsn); err != nil {
		slog.Error("migrate failed", "err", err)
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

	w := worker.New(worker.Config{
		Stage:             *stage,
		WorkerID:          worker.NewWorkerID(),
		Store:             store.New(pool),
		Queue:             queue.New(redis),
		Handler:           worker.NoopHandler{},
		PopTimeout:        30 * time.Second,
		HeartbeatTTL:      15 * time.Second,
		HeartbeatInterval: 5 * time.Second,
	})

	slog.Info("worker starting", "stage", *stage)
	if err := w.Run(ctx); err != nil {
		slog.Error("worker run", "err", err)
		os.Exit(1)
	}
	slog.Info("worker stopped")
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
