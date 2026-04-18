package store

import (
	"context"
	"embed"
	"fmt"

	"github.com/golang-migrate/migrate/v4"
	pgxdriver "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func Migrate(ctx context.Context, dsn string) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return fmt.Errorf("parse dsn: %w", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()

	driver, err := pgxdriver.WithInstance(db, &pgxdriver.Config{})
	if err != nil {
		return fmt.Errorf("driver: %w", err)
	}

	src, err := iofs.New(migrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("source: %w", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "pgx", driver)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("up: %w", err)
	}
	return nil
}
