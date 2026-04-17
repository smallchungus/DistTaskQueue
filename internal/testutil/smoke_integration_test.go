//go:build integration

package testutil

import (
	"context"
	"testing"
)

func TestStartPostgres_AcceptsQueries(t *testing.T) {
	pool := StartPostgres(t)
	var n int
	if err := pool.QueryRow(context.Background(), "SELECT 1").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("got %d, want 1", n)
	}
}

func TestStartRedis_AcceptsCommands(t *testing.T) {
	cli := StartRedis(t)
	if err := cli.Set(context.Background(), "k", "v", 0).Err(); err != nil {
		t.Fatal(err)
	}
	got, err := cli.Get(context.Background(), "k").Result()
	if err != nil {
		t.Fatal(err)
	}
	if got != "v" {
		t.Fatalf("got %q, want %q", got, "v")
	}
}
