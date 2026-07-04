//go:build integration

package backfill_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/backfill"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

func setupFull(t *testing.T, listJSON string) (*store.Store, *queue.Queue, store.User, []byte, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(listJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	email := fmt.Sprintf("full+%d@example.com", time.Now().UnixNano())
	u, _ := s.CreateUser(context.Background(), email)
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}
	return s, q, u, key, srv
}

func fullConfig(s *store.Store, q *queue.Queue, u store.User, key []byte, srv *httptest.Server) backfill.FullConfig {
	return backfill.FullConfig{
		Store:         s,
		Queue:         q,
		EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: srv.URL,
		Email:         u.Email,
		PollInterval:  50 * time.Millisecond,
	}
}

func TestRunFull_EnqueuesAcrossPages(t *testing.T) {
	page1, _ := json.Marshal(map[string]any{
		"messages":      []map[string]any{{"id": "p1"}, {"id": "p2"}},
		"nextPageToken": "TOK",
	})
	page2, _ := json.Marshal(map[string]any{"messages": []map[string]any{{"id": "p3"}}})

	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "TOK" {
			_, _ = w.Write(page2)
			return
		}
		_, _ = w.Write(page1)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))
	email := fmt.Sprintf("pages+%d@example.com", time.Now().UnixNano())
	u, _ := s.CreateUser(context.Background(), email)
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}

	enqueued, skipped, err := backfill.RunFull(context.Background(), fullConfig(s, q, u, key, srv))
	if err != nil {
		t.Fatalf("run full: %v", err)
	}
	if enqueued != 3 || skipped != 0 {
		t.Fatalf("enqueued=%d skipped=%d, want 3/0", enqueued, skipped)
	}
	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 3 {
		t.Fatalf("queue depth: got %d, want 3", depth)
	}
}

func TestRunFull_SecondRunSkipsEverything(t *testing.T) {
	listResp, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"id": "s1"}, {"id": "s2"}, {"id": "s3"}},
	})
	s, q, u, key, srv := setupFull(t, string(listResp))
	cfg := fullConfig(s, q, u, key, srv)

	enqueued, skipped, err := backfill.RunFull(context.Background(), cfg)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if enqueued != 3 || skipped != 0 {
		t.Fatalf("first run enqueued=%d skipped=%d, want 3/0", enqueued, skipped)
	}

	enqueued, skipped, err = backfill.RunFull(context.Background(), cfg)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if enqueued != 0 || skipped != 3 {
		t.Fatalf("second run enqueued=%d skipped=%d, want 0/3", enqueued, skipped)
	}
}

func TestRunFull_BackpressureBlocksUntilDepthDrains(t *testing.T) {
	listResp, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{{"id": "bp-1"}, {"id": "bp-2"}},
	})
	s, q, u, key, srv := setupFull(t, string(listResp))
	cfg := fullConfig(s, q, u, key, srv)
	cfg.MaxQueueDepth = 1

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := q.Push(ctx, "fetch", fmt.Sprintf("seed-%d", i)); err != nil {
			t.Fatal(err)
		}
	}

	done := make(chan struct{})
	var enqueued, skipped int
	var runErr error
	go func() {
		enqueued, skipped, runErr = backfill.RunFull(ctx, cfg)
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("run full returned before queue drained")
	case <-time.After(200 * time.Millisecond):
	}

	for i := 0; i < 3; i++ {
		if _, err := q.BlockingPop(ctx, "fetch", "drain-worker", time.Second); err != nil {
			t.Fatal(err)
		}
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("run full did not resume after queue drained")
	}
	if runErr != nil {
		t.Fatalf("run full: %v", runErr)
	}
	if enqueued != 2 || skipped != 0 {
		t.Fatalf("enqueued=%d skipped=%d, want 2/0", enqueued, skipped)
	}
}
