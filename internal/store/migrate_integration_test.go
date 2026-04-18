//go:build integration

package store_test

import (
	"context"
	"testing"

	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func TestMigrate_AppliesAllPhase3ATables(t *testing.T) {
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	wantTables := []string{"users", "oauth_tokens", "gmail_sync_state", "processed_emails", "pipeline_jobs", "job_status_history"}
	for _, table := range wantTables {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name=$1`, table,
		).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("table %s missing", table)
		}
	}

	wantCols := []string{"user_id", "gmail_message_id", "is_synthetic"}
	for _, col := range wantCols {
		var n int
		err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM information_schema.columns
			 WHERE table_schema='public' AND table_name='pipeline_jobs' AND column_name=$1`, col,
		).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("pipeline_jobs.%s missing", col)
		}
	}
}

func TestMigrate_AppliesInitialSchema(t *testing.T) {
	pool := testutil.StartPostgres(t)

	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM information_schema.tables WHERE table_schema='public' AND table_name IN ('pipeline_jobs','job_status_history')`,
	).Scan(&n)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("expected 2 tables, got %d", n)
	}
}
