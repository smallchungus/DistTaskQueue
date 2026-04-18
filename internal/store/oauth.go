package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrOAuthTokenNotFound = errors.New("oauth token not found")

func (s *Store) SaveOAuthToken(ctx context.Context, t OAuthToken) error {
	const q = `
		INSERT INTO oauth_tokens (user_id, provider, access_ct, refresh_ct, expires_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (user_id, provider) DO UPDATE
		SET access_ct = EXCLUDED.access_ct,
		    refresh_ct = EXCLUDED.refresh_ct,
		    expires_at = EXCLUDED.expires_at,
		    updated_at = now()`

	if _, err := s.pool.Exec(ctx, q, t.UserID, t.Provider, t.AccessCT, t.RefreshCT, t.ExpiresAt); err != nil {
		return fmt.Errorf("save oauth: %w", err)
	}
	return nil
}

func (s *Store) GetOAuthToken(ctx context.Context, userID uuid.UUID, provider string) (OAuthToken, error) {
	const q = `
		SELECT user_id, provider, access_ct, refresh_ct, expires_at, updated_at
		FROM oauth_tokens
		WHERE user_id = $1 AND provider = $2`

	var t OAuthToken
	err := s.pool.QueryRow(ctx, q, userID, provider).Scan(
		&t.UserID, &t.Provider, &t.AccessCT, &t.RefreshCT, &t.ExpiresAt, &t.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OAuthToken{}, ErrOAuthTokenNotFound
	}
	if err != nil {
		return OAuthToken{}, fmt.Errorf("get oauth: %w", err)
	}
	return t, nil
}
