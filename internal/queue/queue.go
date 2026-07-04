package queue

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

func processingKey(stage, workerID string) string {
	return "processing:" + stage + ":" + workerID
}

func (q *Queue) Push(ctx context.Context, stage, jobID string) error {
	if err := q.cli.LPush(ctx, key(stage), jobID).Err(); err != nil {
		return fmt.Errorf("push: %w", err)
	}
	return nil
}

func (q *Queue) BlockingPop(ctx context.Context, stage, workerID string, timeout time.Duration) (string, error) {
	res, err := q.cli.BLMove(ctx, key(stage), processingKey(stage, workerID), "RIGHT", "LEFT", timeout).Result()
	if errors.Is(err, goredis.Nil) {
		return "", ErrEmpty
	}
	if err != nil {
		return "", fmt.Errorf("pop: %w", err)
	}
	return res, nil
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

func (q *Queue) IsWorkerAlive(ctx context.Context, workerID string) (bool, error) {
	n, err := q.cli.Exists(ctx, "heartbeat:"+workerID).Result()
	if err != nil {
		return false, fmt.Errorf("exists: %w", err)
	}
	return n > 0, nil
}

func (q *Queue) Heartbeat(ctx context.Context, workerID string, ttl time.Duration) error {
	now := strconv.FormatInt(time.Now().Unix(), 10)
	if err := q.cli.Set(ctx, "heartbeat:"+workerID, now, ttl).Err(); err != nil {
		return fmt.Errorf("heartbeat: %w", err)
	}
	return nil
}

func (q *Queue) Ack(ctx context.Context, stage, workerID, jobID string) error {
	if err := q.cli.LRem(ctx, processingKey(stage, workerID), 1, jobID).Err(); err != nil {
		return fmt.Errorf("ack: %w", err)
	}
	return nil
}

type ProcessingRef struct {
	Stage    string
	WorkerID string
}

func (q *Queue) ListProcessing(ctx context.Context) ([]ProcessingRef, error) {
	var out []ProcessingRef
	var cursor uint64
	for {
		keys, next, err := q.cli.Scan(ctx, cursor, "processing:*", 100).Result()
		if err != nil {
			return nil, fmt.Errorf("scan processing: %w", err)
		}
		for _, k := range keys {
			parts := strings.SplitN(k, ":", 3)
			if len(parts) != 3 {
				continue
			}
			out = append(out, ProcessingRef{Stage: parts[1], WorkerID: parts[2]})
		}
		cursor = next
		if cursor == 0 {
			return out, nil
		}
	}
}

func (q *Queue) ReclaimProcessing(ctx context.Context, stage, workerID string) (int, error) {
	moved := 0
	for {
		_, err := q.cli.LMove(ctx, processingKey(stage, workerID), key(stage), "RIGHT", "RIGHT").Result()
		if errors.Is(err, goredis.Nil) {
			return moved, nil
		}
		if err != nil {
			return moved, fmt.Errorf("reclaim: %w", err)
		}
		moved++
	}
}
