package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	const q = `SELECT id, email, created_at FROM users ORDER BY created_at`
	rows, err := s.pool.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		out = append(out, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows: %w", err)
	}
	return out, nil
}

// GetGmailSyncState returns "" if no row exists for this user.
func (s *Store) GetGmailSyncState(ctx context.Context, userID uuid.UUID) (string, error) {
	const q = `SELECT history_id FROM gmail_sync_state WHERE user_id = $1`

	var hid string
	err := s.pool.QueryRow(ctx, q, userID).Scan(&hid)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get sync state: %w", err)
	}
	return hid, nil
}

func (s *Store) SetGmailSyncState(ctx context.Context, userID uuid.UUID, historyID string) error {
	const q = `
		INSERT INTO gmail_sync_state (user_id, history_id, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (user_id) DO UPDATE
		SET history_id = EXCLUDED.history_id, updated_at = now()`

	if _, err := s.pool.Exec(ctx, q, userID, historyID); err != nil {
		return fmt.Errorf("set sync state: %w", err)
	}
	return nil
}

// HasJobForMessage returns true if a pipeline_jobs row already exists for
// (user_id, gmail_message_id) in ANY status. Backfill uses this as the
// idempotency check before re-enqueueing a message the scheduler already
// knows about.
func (s *Store) HasJobForMessage(ctx context.Context, userID uuid.UUID, gmailMessageID string) (bool, error) {
	const q = `SELECT 1 FROM pipeline_jobs WHERE user_id = $1 AND gmail_message_id = $2 LIMIT 1`
	var one int
	err := s.pool.QueryRow(ctx, q, userID, gmailMessageID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("has job for message: %w", err)
	}
	return true, nil
}
