package drive

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	goredis "github.com/redis/go-redis/v9"
)

type FolderCache struct {
	cli *goredis.Client
}

func NewFolderCache(cli *goredis.Client) *FolderCache {
	return &FolderCache{cli: cli}
}

func (c *FolderCache) Get(ctx context.Context, userID uuid.UUID, path string) (string, bool, error) {
	key := fmt.Sprintf("drive_folder_cache:%s:%s", userID, path)
	val, err := c.cli.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("cache get: %w", err)
	}
	return val, true, nil
}

func (c *FolderCache) Set(ctx context.Context, userID uuid.UUID, path, folderID string, ttl time.Duration) error {
	key := fmt.Sprintf("drive_folder_cache:%s:%s", userID, path)
	if err := c.cli.Set(ctx, key, folderID, ttl).Err(); err != nil {
		return fmt.Errorf("cache set: %w", err)
	}
	return nil
}
