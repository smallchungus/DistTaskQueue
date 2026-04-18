//go:build integration

package drive_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/smallchungus/disttaskqueue/internal/drive"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func TestFolderCache_SetThenGet(t *testing.T) {
	cli := testutil.StartRedis(t)
	cache := drive.NewFolderCache(cli)
	ctx := context.Background()
	uid := uuid.New()

	if err := cache.Set(ctx, uid, "2026/04/17", "folder-id-abc", time.Minute); err != nil {
		t.Fatal(err)
	}

	got, ok, err := cache.Get(ctx, uid, "2026/04/17")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got != "folder-id-abc" {
		t.Fatalf("got %q, ok=%v", got, ok)
	}
}

func TestFolderCache_GetReturnsFalseOnMiss(t *testing.T) {
	cli := testutil.StartRedis(t)
	cache := drive.NewFolderCache(cli)
	ctx := context.Background()
	uid := uuid.New()

	got, ok, err := cache.Get(ctx, uid, "nope")
	if err != nil {
		t.Fatal(err)
	}
	if ok || got != "" {
		t.Fatalf("got %q, ok=%v, want miss", got, ok)
	}
}
