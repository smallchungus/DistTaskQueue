package api

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

// metricsCollector exposes DistTaskQueue-specific Prometheus gauges. The
// values are refreshed by a background goroutine at a fixed interval; HTTP
// scrapes read the cached values, so /metrics stays fast even if the
// underlying Postgres / Redis queries get slow.
type metricsCollector struct {
	store *store.Store
	queue *queue.Queue
	redis *goredis.Client

	queueDepth   *prometheus.GaugeVec
	jobCount     *prometheus.GaugeVec
	aliveWorkers prometheus.Gauge
	scrapeErrors prometheus.Counter

	mu      sync.Mutex
	running bool
}

func newMetricsCollector(s *store.Store, q *queue.Queue, r *goredis.Client) *metricsCollector {
	return &metricsCollector{
		store: s,
		queue: q,
		redis: r,
		queueDepth: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dtq_queue_depth",
				Help: "Current depth of each Redis stage queue (LLEN).",
			},
			[]string{"stage"},
		),
		jobCount: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "dtq_job_count",
				Help: "Count of pipeline_jobs rows in each status.",
			},
			[]string{"status"},
		),
		aliveWorkers: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "dtq_alive_workers",
			Help: "Count of worker heartbeat keys currently in Redis.",
		}),
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "dtq_metrics_scrape_errors_total",
			Help: "Errors encountered while refreshing DistTaskQueue metrics.",
		}),
	}
}

func (m *metricsCollector) register(reg prometheus.Registerer) {
	reg.MustRegister(m.queueDepth, m.jobCount, m.aliveWorkers, m.scrapeErrors)
}

// start begins the refresh loop. Safe to call once; subsequent calls are noops.
func (m *metricsCollector) start(ctx context.Context, interval time.Duration) {
	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return
	}
	m.running = true
	m.mu.Unlock()

	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		m.refresh(ctx) // prime values before first scrape
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.refresh(ctx)
			}
		}
	}()
}

func (m *metricsCollector) refresh(ctx context.Context) {
	for _, stage := range []string{"fetch", "render", "upload", "test"} {
		n, err := m.queue.Depth(ctx, stage)
		if err != nil {
			m.scrapeErrors.Inc()
			slog.Warn("metrics queue depth", "stage", stage, "err", err)
			continue
		}
		m.queueDepth.WithLabelValues(stage).Set(float64(n))
	}

	const q = `SELECT status, count(*) FROM pipeline_jobs GROUP BY status`
	rows, err := m.store.PoolForTest().Query(ctx, q)
	if err != nil {
		m.scrapeErrors.Inc()
		slog.Warn("metrics job counts", "err", err)
	} else {
		// Zero out known statuses before refreshing so a status that drops to
		// zero is correctly reported as 0 rather than stuck at the last seen
		// non-zero value.
		for _, s := range []string{"queued", "running", "done", "dead"} {
			m.jobCount.WithLabelValues(s).Set(0)
		}
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err == nil {
				m.jobCount.WithLabelValues(status).Set(float64(n))
			}
		}
		rows.Close()
	}

	alive := 0
	var cursor uint64
	for {
		keys, next, err := m.redis.Scan(ctx, cursor, "heartbeat:*", 100).Result()
		if err != nil {
			m.scrapeErrors.Inc()
			slog.Warn("metrics heartbeat scan", "err", err)
			break
		}
		alive += len(keys)
		cursor = next
		if cursor == 0 {
			break
		}
	}
	m.aliveWorkers.Set(float64(alive))
}

// metricsHandler returns an HTTP handler serving Prometheus exposition format
// with DistTaskQueue metrics plus the default Go runtime metrics.
func metricsHandler(reg *prometheus.Registry) http.Handler {
	return promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg})
}
