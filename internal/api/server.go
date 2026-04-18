package api

import (
	"context"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
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
		mountDemo(r, cfg.Store, cfg.Queue)

		// Prometheus /metrics. A fresh Registry (instead of the default global)
		// so tests don't leak metric registrations across runs.
		reg := prometheus.NewRegistry()
		reg.MustRegister(
			collectors.NewGoCollector(),
			collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		)
		mc := newMetricsCollector(cfg.Store, cfg.Queue, cfg.Redis)
		mc.register(reg)
		// Background ctx: metrics collector lives for the life of the process.
		// Clean shutdown happens when the process exits; there's no intermediate
		// state to preserve so no need to thread a cancellable ctx through.
		mc.start(context.Background(), 10*time.Second)
		r.Method("GET", "/metrics", metricsHandler(reg))
	}

	return r
}
