package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/api"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

var (
	version = "dev"
	commit  = "none"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	addr := envOr("API_ADDR", ":8080")
	dsn := envOr("DATABASE_URL", "")
	redisURL := envOr("REDIS_URL", "")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := api.Config{Version: version, Commit: commit}

	// If DATABASE_URL and REDIS_URL are set, expose dashboard endpoints.
	// When unset (bare /healthz mode), router still works for probes.
	if dsn != "" && redisURL != "" {
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

		cfg.Store = store.New(pool)
		cfg.Queue = queue.New(redis)
		cfg.Redis = redis
		slog.Info("dashboard enabled")
	} else {
		slog.Info("dashboard disabled (DATABASE_URL and REDIS_URL not both set)")
	}

	srv := &http.Server{
		Addr:              addr,
		Handler:           api.NewRouter(cfg),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		slog.Info("api listening", "addr", addr, "version", version, "commit", commit)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("listen failed", "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	slog.Info("shutting down")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("shutdown failed", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
