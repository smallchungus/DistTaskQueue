package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var ErrUserNotFound = errors.New("user not found")

func (s *Store) CreateUser(ctx context.Context, email string) (User, error) {
	id := uuid.New()
	const q = `
		INSERT INTO users (id, email) VALUES ($1, $2)
		RETURNING id, email, created_at`

	var u User
	if err := s.pool.QueryRow(ctx, q, id, email).Scan(&u.ID, &u.Email, &u.CreatedAt); err != nil {
		return User{}, fmt.Errorf("create user: %w", err)
	}
	return u, nil
}

func (s *Store) GetUserByEmail(ctx context.Context, email string) (User, error) {
	const q = `SELECT id, email, created_at FROM users WHERE email = $1`

	var u User
	err := s.pool.QueryRow(ctx, q, email).Scan(&u.ID, &u.Email, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	}
	if err != nil {
		return User{}, fmt.Errorf("get user: %w", err)
	}
	return u, nil
}
