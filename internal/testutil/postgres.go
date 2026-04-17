//go:build integration

package testutil

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
)

func StartPostgres(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	c, err := postgres.Run(ctx, "postgres:16-alpine",
		postgres.WithDatabase("dtq_test"),
		postgres.WithUsername("dtq"),
		postgres.WithPassword("dtq"),
		postgres.BasicWaitStrategies(),
		postgres.WithSQLDriver("pgx"),
	)
	if err != nil {
		t.Fatalf("postgres start: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Terminate(context.Background())
	})

	dsn, err := c.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("dsn: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	cfg.MaxConnIdleTime = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return pool
}
