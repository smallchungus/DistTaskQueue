package main

import (
	"context"
	"encoding/base64"
	"flag"
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

	"github.com/smallchungus/disttaskqueue/internal/handler"
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

	var h worker.Handler
	switch *stage {
	case "test":
		h = worker.NoopHandler{}
	case "fetch":
		cfg, key := loadGoogleCfgAndKey()
		h = handler.NewFetchHandler(handler.FetchConfig{
			Store:         store.New(pool),
			EncryptionKey: key,
			OAuth2:        cfg,
			DataDir:       envOr("DATA_DIR", "/data"),
		})
	case "render":
		h = handler.NewRenderHandler(handler.RenderConfig{
			DataDir:     envOr("DATA_DIR", "/data"),
			PDFEndpoint: envOr("GOTENBERG_URL", "http://gotenberg:3000"),
		})
	case "upload":
		cfg, key := loadGoogleCfgAndKey()
		h = handler.NewUploadHandler(handler.UploadConfig{
			Store:         store.New(pool),
			EncryptionKey: key,
			OAuth2:        cfg,
			Redis:         redis,
			DataDir:       envOr("DATA_DIR", "/data"),
			RootFolderID:  envOr("DRIVE_ROOT_FOLDER_ID", ""),
			RootPath:      envOr("DRIVE_ROOT_PATH", ""),
		})
	default:
		slog.Error("unknown stage", "stage", *stage)
		os.Exit(2)
	}

	w := worker.New(worker.Config{
		Stage:             *stage,
		WorkerID:          worker.NewWorkerID(),
		Store:             store.New(pool),
		Queue:             queue.New(redis),
		Handler:           h,
		PopTimeout:        envDurationSec("WORKER_POP_TIMEOUT_SEC", 30),
		HeartbeatTTL:      envDurationSec("WORKER_HEARTBEAT_TTL_SEC", 15),
		HeartbeatInterval: envDurationSec("WORKER_HEARTBEAT_INTERVAL_SEC", 5),
	})

	slog.Info("worker starting", "stage", *stage)
	if err := w.Run(ctx); err != nil {
		slog.Error("worker run", "err", err)
		os.Exit(1)
	}
	slog.Info("worker stopped")
}

func loadGoogleCfgAndKey() (*oauth2.Config, []byte) {
	clientID := mustEnv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := mustEnv("GOOGLE_OAUTH_CLIENT_SECRET")
	keyB64 := mustEnv("TOKEN_ENCRYPTION_KEY")
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		slog.Error("decode TOKEN_ENCRYPTION_KEY", "err", err)
		os.Exit(1)
	}
	if len(key) != 32 {
		slog.Error("TOKEN_ENCRYPTION_KEY must decode to 32 bytes", "got", len(key))
		os.Exit(1)
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
	}, key
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing env var", "key", k)
		os.Exit(1)
	}
	return v
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envDurationSec reads an env var as seconds. Non-positive / invalid values
// fall back to def. Required so timeouts can be tuned without recompiling.
func envDurationSec(k string, def int) time.Duration {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return time.Duration(def) * time.Second
}
