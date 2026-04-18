package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

var ErrJobNotFound = errors.New("job not found")

func (s *Store) EnqueueJob(ctx context.Context, nj NewJob) (Job, error) {
	id := uuid.New()
	payload := nj.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}

	const q = `
		INSERT INTO pipeline_jobs (id, user_id, gmail_message_id, stage, status, payload, is_synthetic)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
		          worker_id, attempts, max_attempts, last_error, next_run_at,
		          claimed_at, completed_at, created_at, updated_at`

	row := s.pool.QueryRow(ctx, q, id, nj.UserID, nj.GmailMessageID, nj.Stage, StatusQueued, payload, nj.IsSynthetic)
	var j Job
	if err := row.Scan(&j.ID, &j.UserID, &j.GmailMessageID, &j.Stage, &j.Status, &j.Payload, &j.IsSynthetic,
		&j.WorkerID, &j.Attempts, &j.MaxAttempts, &j.LastError, &j.NextRunAt,
		&j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt); err != nil {
		return Job{}, fmt.Errorf("enqueue: %w", err)
	}
	return j, nil
}

var ErrJobNotClaimable = errors.New("job not claimable")

func (s *Store) ClaimJob(ctx context.Context, id uuid.UUID, workerID string) error {
	const q = `
		UPDATE pipeline_jobs
		SET status = $1, worker_id = $2, claimed_at = now(), updated_at = now()
		WHERE id = $3 AND status = $4
		RETURNING id`

	var got uuid.UUID
	err := s.pool.QueryRow(ctx, q, StatusRunning, workerID, id, StatusQueued).Scan(&got)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrJobNotClaimable
	}
	if err != nil {
		return fmt.Errorf("claim: %w", err)
	}
	return nil
}

func (s *Store) MarkDone(ctx context.Context, id uuid.UUID) error {
	const q = `
		UPDATE pipeline_jobs
		SET status = $1, completed_at = now(), updated_at = now()
		WHERE id = $2`

	tag, err := s.pool.Exec(ctx, q, StatusDone, id)
	if err != nil {
		return fmt.Errorf("mark done: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}

func (s *Store) MarkFailed(ctx context.Context, id uuid.UUID, errMsg string, nextRunAt time.Time) error {
	const q = `
		UPDATE pipeline_jobs
		SET attempts = attempts + 1,
		    status = CASE WHEN attempts + 1 >= max_attempts THEN $1::text ELSE $2::text END,
		    worker_id = NULL,
		    last_error = $3,
		    next_run_at = $4,
		    updated_at = now()
		WHERE id = $5`

	tag, err := s.pool.Exec(ctx, q, StatusDead, StatusQueued, errMsg, nextRunAt, id)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}

func (s *Store) ListRunningJobs(ctx context.Context) ([]Job, error) {
	const q = `
		SELECT id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
		       worker_id, attempts, max_attempts, last_error, next_run_at,
		       claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs WHERE status = $1`
	rows, err := s.pool.Query(ctx, q, StatusRunning)
	if err != nil {
		return nil, fmt.Errorf("list running: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) ListReadyRetryJobs(ctx context.Context) ([]Job, error) {
	const q = `
		SELECT id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
		       worker_id, attempts, max_attempts, last_error, next_run_at,
		       claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs
		WHERE status = $1 AND last_error IS NOT NULL AND next_run_at <= now()`
	rows, err := s.pool.Query(ctx, q, StatusQueued)
	if err != nil {
		return nil, fmt.Errorf("list ready retries: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func scanJobs(rows pgx.Rows) ([]Job, error) {
	var out []Job
	for rows.Next() {
		var j Job
		if err := rows.Scan(&j.ID, &j.UserID, &j.GmailMessageID, &j.Stage, &j.Status, &j.Payload, &j.IsSynthetic,
			&j.WorkerID, &j.Attempts, &j.MaxAttempts, &j.LastError, &j.NextRunAt,
			&j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

func (s *Store) AdvanceJob(ctx context.Context, id uuid.UUID, nextStage string) error {
	const q = `
		UPDATE pipeline_jobs
		SET stage = $1, status = $2, worker_id = NULL, claimed_at = NULL, updated_at = now()
		WHERE id = $3 AND status = $4`

	tag, err := s.pool.Exec(ctx, q, nextStage, StatusQueued, id, StatusRunning)
	if err != nil {
		return fmt.Errorf("advance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}

func (s *Store) PoolForTest() *pgxpool.Pool { return s.pool }

func (s *Store) ListStaleQueuedJobs(ctx context.Context, threshold time.Duration) ([]Job, error) {
	const q = `
		SELECT id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
		       worker_id, attempts, max_attempts, last_error, next_run_at,
		       claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs
		WHERE status = $1 AND last_error IS NULL
		  AND created_at < now() - make_interval(secs => $2)`

	rows, err := s.pool.Query(ctx, q, StatusQueued, threshold.Seconds())
	if err != nil {
		return nil, fmt.Errorf("list stale queued: %w", err)
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) GetJob(ctx context.Context, id uuid.UUID) (Job, error) {
	const q = `
		SELECT id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
		       worker_id, attempts, max_attempts, last_error, next_run_at,
		       claimed_at, completed_at, created_at, updated_at
		FROM pipeline_jobs WHERE id = $1`

	row := s.pool.QueryRow(ctx, q, id)
	var j Job
	err := row.Scan(&j.ID, &j.UserID, &j.GmailMessageID, &j.Stage, &j.Status, &j.Payload, &j.IsSynthetic,
		&j.WorkerID, &j.Attempts, &j.MaxAttempts, &j.LastError, &j.NextRunAt,
		&j.ClaimedAt, &j.CompletedAt, &j.CreatedAt, &j.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Job{}, ErrJobNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("get: %w", err)
	}
	return j, nil
}
