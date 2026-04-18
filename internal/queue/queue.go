package queue

import (
	"context"
	"fmt"

	goredis "github.com/redis/go-redis/v9"
)

type Queue struct {
	cli *goredis.Client
}

func New(cli *goredis.Client) *Queue {
	return &Queue{cli: cli}
}

func key(stage string) string {
	return "queue:" + stage
}

func (q *Queue) Push(ctx context.Context, stage, jobID string) error {
	if err := q.cli.LPush(ctx, key(stage), jobID).Err(); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func (q *Queue) Depth(ctx context.Context, stage string) (int64, error) {
	n, err := q.cli.LLen(ctx, key(stage)).Result()
	if err != nil {
		return 0, fmt.Errorf("depth: %w", err)
	}
	return n, nil
}
