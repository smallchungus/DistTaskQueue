package api

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

//go:embed dashboard.html
var dashboardHTML []byte

type dashboard struct {
	store *store.Store
	queue *queue.Queue
	redis *goredis.Client
}

type statsResponse struct {
	QueueDepths  map[string]int64 `json:"queue_depths"`
	JobCounts    map[string]int   `json:"job_counts"`
	AliveWorkers []string         `json:"alive_workers"`
	ServerTime   time.Time        `json:"server_time"`
}

// Ensure *queue.Queue satisfies queuePusher so mountDemo can accept it.
var _ queuePusher = (*queue.Queue)(nil)

type jobSummary struct {
	ID             string     `json:"id"`
	Stage          string     `json:"stage"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	LastError      string     `json:"last_error,omitempty"`
	GmailMessageID string     `json:"gmail_message_id,omitempty"`
	CreatedAt      time.Time  `json:"created_at"`
	CompletedAt    *time.Time `json:"completed_at,omitempty"`
}

type jobsResponse struct {
	Jobs []jobSummary `json:"jobs"`
}

func (d *dashboard) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(dashboardHTML)
}

func (d *dashboard) stats(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	resp := statsResponse{
		QueueDepths:  map[string]int64{},
		JobCounts:    map[string]int{},
		AliveWorkers: []string{},
		ServerTime:   time.Now().UTC(),
	}

	for _, stage := range []string{"fetch", "render", "upload", "test"} {
		n, err := d.queue.Depth(ctx, stage)
		if err != nil {
			writeJSONErr(w, http.StatusInternalServerError, "queue depth: "+err.Error())
			return
		}
		resp.QueueDepths[stage] = n
	}

	counts, err := d.jobCounts(ctx)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "job counts: "+err.Error())
		return
	}
	resp.JobCounts = counts

	workers, err := d.aliveWorkers(ctx)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "alive workers: "+err.Error())
		return
	}
	resp.AliveWorkers = workers

	writeJSON(w, http.StatusOK, resp)
}

func (d *dashboard) recentJobs(w http.ResponseWriter, r *http.Request) {
	limit := 20
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}

	jobs, err := d.listRecentJobs(r.Context(), limit)
	if err != nil {
		writeJSONErr(w, http.StatusInternalServerError, "recent jobs: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, jobsResponse{Jobs: jobs})
}

func (d *dashboard) jobCounts(ctx context.Context) (map[string]int, error) {
	const q = `SELECT status, count(*) FROM pipeline_jobs GROUP BY status`
	rows, err := d.store.PoolForTest().Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{"queued": 0, "running": 0, "done": 0, "dead": 0}
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return nil, err
		}
		out[status] = n
	}
	return out, rows.Err()
}

func (d *dashboard) aliveWorkers(ctx context.Context) ([]string, error) {
	var cursor uint64
	workers := []string{}
	for {
		keys, next, err := d.redis.Scan(ctx, cursor, "heartbeat:*", 100).Result()
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			workers = append(workers, k[len("heartbeat:"):])
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return workers, nil
}

func (d *dashboard) listRecentJobs(ctx context.Context, limit int) ([]jobSummary, error) {
	const q = `
		SELECT id, stage, status, attempts, COALESCE(last_error, ''),
		       COALESCE(gmail_message_id, ''), created_at, completed_at
		FROM pipeline_jobs
		ORDER BY created_at DESC
		LIMIT $1`
	rows, err := d.store.PoolForTest().Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []jobSummary{}
	for rows.Next() {
		var j jobSummary
		if err := rows.Scan(&j.ID, &j.Stage, &j.Status, &j.Attempts, &j.LastError,
			&j.GmailMessageID, &j.CreatedAt, &j.CompletedAt); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeJSONErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
