package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Version string
	Commit  string
	// Store and Queue are optional — when non-nil the router exposes dashboard
	// endpoints (/, /api/stats, /api/jobs/recent) that read from them.
	Store *store.Store
	Queue *queue.Queue
	Redis *goredis.Client
}

func NewRouter(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", healthz)
	r.Get("/version", versionHandler(cfg.Version, cfg.Commit))

	if cfg.Store != nil && cfg.Queue != nil && cfg.Redis != nil {
		d := &dashboard{store: cfg.Store, queue: cfg.Queue, redis: cfg.Redis}
		r.Get("/", d.index)
		r.Get("/api/stats", d.stats)
		r.Get("/api/jobs/recent", d.recentJobs)
	}

	return r
}
