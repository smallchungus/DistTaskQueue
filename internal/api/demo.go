package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

// demoConfig tunes the demo-trigger endpoints. Each field is optional; zero
// values fall back to sensible defaults.
type demoConfig struct {
	// Per-IP rate limit: sustained 1 call/sec with burst 5 by default.
	// FloodCooldown is a separate gate specific to the /flood endpoint to
	// prevent repeated 1000-job dumps.
	FloodCooldown time.Duration
	// BurstSize is the default count for the /burst endpoint.
	BurstSize int
	// FloodSize is the fixed count for /flood.
	FloodSize int
	// SlowSleepMs is how long the /slow job sleeps.
	SlowSleepMs int
}

func defaultDemoConfig() demoConfig {
	return demoConfig{
		FloodCooldown: 60 * time.Second,
		BurstSize:     100,
		FloodSize:     1000,
		SlowSleepMs:   5000,
	}
}

type demoRoutes struct {
	store      *store.Store
	queue      queuePusher
	rl         *tokenBucket
	floodGate  *tokenBucket // global "once every 60s" gate for the flood button
	cfg        demoConfig
	synthStage string
}

// queuePusher is the tiny surface of *queue.Queue that demo endpoints need.
// Kept as an interface so we don't leak queue internals here.
type queuePusher interface {
	Push(ctx context.Context, stage, jobID string) error
}

// mountDemo registers POST endpoints that enqueue synthetic jobs. All are
// per-IP rate-limited. Synthetic jobs are tagged is_synthetic=true and use
// a dedicated stage ("test") so the test-stage worker running NoopHandler
// picks them up without touching real Gmail/Drive code paths.
func mountDemo(r interface {
	Post(string, http.HandlerFunc)
}, s *store.Store, q queuePusher) {
	d := &demoRoutes{
		store:      s,
		queue:      q,
		rl:         newTokenBucket(1*time.Second, 5),
		floodGate:  newTokenBucket(60*time.Second, 1),
		cfg:        defaultDemoConfig(),
		synthStage: "test",
	}
	r.Post("/api/demo/burst", d.rl.rateLimit(d.burst))
	r.Post("/api/demo/slow", d.rl.rateLimit(d.slow))
	r.Post("/api/demo/flaky", d.rl.rateLimit(d.flaky))
	r.Post("/api/demo/flood", d.rl.rateLimit(d.flood))
}

func (d *demoRoutes) burst(w http.ResponseWriter, r *http.Request) {
	n := d.cfg.BurstSize
	if v := r.URL.Query().Get("n"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 && parsed <= 500 {
			n = parsed
		}
	}
	count := d.enqueueSynthetic(r.Context(), n, `{"sleepMs":25}`)
	writeJSON(w, http.StatusOK, map[string]any{"enqueued": count})
}

func (d *demoRoutes) slow(w http.ResponseWriter, r *http.Request) {
	payload := fmt.Sprintf(`{"sleepMs":%d}`, d.cfg.SlowSleepMs)
	count := d.enqueueSynthetic(r.Context(), 1, payload)
	writeJSON(w, http.StatusOK, map[string]any{"enqueued": count})
}

func (d *demoRoutes) flaky(w http.ResponseWriter, r *http.Request) {
	// 50% fail, with a short sleep so we actually see it running before it fails.
	count := d.enqueueSynthetic(r.Context(), 1, `{"sleepMs":100,"failRate":0.5}`)
	writeJSON(w, http.StatusOK, map[string]any{"enqueued": count})
}

func (d *demoRoutes) flood(w http.ResponseWriter, r *http.Request) {
	if !d.floodGate.Allow("global") {
		w.Header().Set("Retry-After", "60")
		writeJSONErr(w, http.StatusTooManyRequests, "flood already fired in last 60s — cooldown in effect")
		return
	}
	count := d.enqueueSynthetic(r.Context(), d.cfg.FloodSize, `{"sleepMs":10}`)
	writeJSON(w, http.StatusOK, map[string]any{"enqueued": count})
}

// enqueueSynthetic creates N pipeline_jobs rows (is_synthetic=true, stage=test)
// and pushes each ID onto queue:test. Returns how many actually got onto the
// queue; DB errors are logged but don't halt the batch.
func (d *demoRoutes) enqueueSynthetic(ctx context.Context, n int, payload string) int {
	pushed := 0
	for i := 0; i < n; i++ {
		j, err := d.store.EnqueueJob(ctx, store.NewJob{
			Stage:       d.synthStage,
			Payload:     json.RawMessage(payload),
			IsSynthetic: true,
		})
		if err != nil {
			slog.Warn("demo enqueue failed", "i", i, "err", err)
			continue
		}
		if err := d.queue.Push(ctx, d.synthStage, j.ID.String()); err != nil {
			slog.Warn("demo push failed", "job_id", j.ID, "err", err)
			continue
		}
		pushed++
	}
	return pushed
}
