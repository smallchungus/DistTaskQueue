package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) EnqueueJob(ctx context.Context, nj NewJob) (Job, error) {
	id := uuid.New()
	payload := nj.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}

	const q = `
		INSERT INTO pipeline_jobs (id, stage, status, payload)
		VALUES ($1, $2, $3, $4)
		RETURNING id, stage, status, payload, worker_id, attempts, max_attempts,
		          last_error, next_run_at, claimed_at, completed_at, created_at, updated_at`

	row := s.pool.QueryRow(ctx, q, id, nj.Stage, StatusQueued, payload)
	var j Job
	if err := row.Scan(&j.ID, &j.Stage, &j.Status, &j.Payload, &j.WorkerID, &j.Attempts,
		&j.MaxAttempts, &j.LastError, &j.NextRunAt, &j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return Job{}, fmt.Errorf("enqueue: %w", err)
	}
	return j, nil
}
