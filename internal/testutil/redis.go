//go:build integration

package testutil

import (
	"context"
	"testing"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go/modules/redis"
)

func StartRedis(t *testing.T) *goredis.Client {
	t.Helper()
	ctx := context.Background()

	c, err := redis.Run(ctx, "redis:7-alpine")
	if err != nil {
		t.Fatalf("redis start: %v", err)
	}
	t.Cleanup(func() {
		_ = c.Terminate(context.Background())
	})

	uri, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("uri: %v", err)
	}
	opts, err := goredis.ParseURL(uri)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cli := goredis.NewClient(opts)
	t.Cleanup(func() { _ = cli.Close() })

	if err := cli.Ping(ctx).Err(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return cli
}
