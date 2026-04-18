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

func newKey32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func setup(t *testing.T, listJSON string) (*backfill.Backfill, *store.Store, *queue.Queue, store.User) {
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

	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("bf+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	_ = oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok)

	b := backfill.New(backfill.Config{
		Store:         s,
		Queue:         q,
		EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: srv.URL,
		Window:        24 * time.Hour,
	})
	return b, s, q, u
}

func TestRunOnce_EnqueuesNewMessages(t *testing.T) {
	listResp, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"id": "bf-1"}, {"id": "bf-2"}, {"id": "bf-3"},
		},
	})

	b, _, q, _ := setup(t, string(listResp))

	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 3 {
		t.Fatalf("queue depth: got %d, want 3", depth)
	}
}

func TestRunOnce_SkipsMessagesAlreadyInPipeline(t *testing.T) {
	listResp, _ := json.Marshal(map[string]any{
		"messages": []map[string]any{
			{"id": "seen-1"}, {"id": "new-1"}, {"id": "seen-2"},
		},
	})

	b, s, q, u := setup(t, string(listResp))

	// Pre-seed two of the three as already-known in pipeline_jobs. EnqueueJob
	// only inserts the DB row, not the Redis push, so queue depth stays at 0
	// before backfill runs.
	for _, msgID := range []string{"seen-1", "seen-2"} {
		id := msgID
		uid := u.ID
		if _, err := s.EnqueueJob(context.Background(), store.NewJob{
			Stage:          "fetch",
			UserID:         &uid,
			GmailMessageID: &id,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 1 {
		t.Fatalf("queue depth: got %d, want 1 (only new-1 should enqueue)", depth)
	}
}

func TestRunOnce_EmptyGmailResponse(t *testing.T) {
	listResp, _ := json.Marshal(map[string]any{"messages": []map[string]any{}})

	b, _, q, _ := setup(t, string(listResp))

	if err := b.RunOnce(context.Background()); err != nil {
		t.Fatalf("run once: %v", err)
	}

	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 0 {
		t.Fatalf("queue depth: got %d, want 0", depth)
	}
}
