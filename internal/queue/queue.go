package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"
)

type Queue struct {
	cli *goredis.Client
}

func New(cli *goredis.Client) *Queue {
	return &Queue{cli: cli}
}

var ErrEmpty = errors.New("queue empty")

func key(stage string) string {
	return "queue:" + stage
}

func (q *Queue) Push(ctx context.Context, stage, jobID string) error {
	if err := q.cli.LPush(ctx, key(stage), jobID).Err(); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func (q *Queue) BlockingPop(ctx context.Context, stage string, timeout time.Duration) (string, error) {
	res, err := q.cli.BRPop(ctx, timeout, key(stage)).Result()
	if errors.Is(err, goredis.Nil) {
		return "", ErrEmpty
	}
	if err != nil {
		return "", fmt.Errorf("pop: %w", err)
	}
	if len(res) != 2 {
		return "", fmt.Errorf("pop: unexpected response shape %v", res)
	}
	return res[1], nil
}

func (q *Queue) Depth(ctx context.Context, stage string) (int64, error) {
	n, err := q.cli.LLen(ctx, key(stage)).Result()
	if err != nil {
		return 0, fmt.Errorf("depth: %w", err)
	}
	return n, nil
}

// Client exposes the underlying Redis client. Test-only.
func (q *Queue) Client() *goredis.Client { return q.cli }

func (q *Queue) Heartbeat(ctx context.Context, workerID string, ttl time.Duration) error {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if err := q.cli.Set(ctx, "heartbeat:"+workerID, now, ttl).Err(); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}
