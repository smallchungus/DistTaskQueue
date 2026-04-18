//go:build integration

package scheduler_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/scheduler"
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

func TestPollOnce_EnqueuesNewMessageJobs(t *testing.T) {
	histResp, _ := json.Marshal(map[string]any{
		"history": []map[string]any{
			{
				"id": "100",
				"messagesAdded": []map[string]any{
					{"message": map[string]any{"id": "msg-1", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
					{"message": map[string]any{"id": "msg-2", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
				},
			},
		},
		"historyId": "150",
	})
	profileResp, _ := json.Marshal(map[string]any{"historyId": "10"})

	gmailMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/gmail/v1/users/me/profile":
			_, _ = w.Write(profileResp)
		case r.URL.Path == "/gmail/v1/users/me/history":
			_, _ = w.Write(histResp)
		default:
			http.NotFound(w, r)
		}
	}))
	defer gmailMock.Close()

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("sched+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}
	// Seed sync state to skip the initial-sync GetProfile path on the FIRST poll.
	if err := s.SetGmailSyncState(context.Background(), u.ID, "1"); err != nil {
		t.Fatal(err)
	}

	sch := scheduler.New(scheduler.Config{
		Store:         s,
		Queue:         q,
		EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: gmailMock.URL,
	})

	if err := sch.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 2 {
		t.Fatalf("queue depth: got %d, want 2", depth)
	}

	cursor, _ := s.GetGmailSyncState(context.Background(), u.ID)
	if cursor != "150" {
		t.Fatalf("cursor: got %q, want 150", cursor)
	}
}

func TestPollOnce_InitializesSyncStateFromProfile(t *testing.T) {
	profileResp, _ := json.Marshal(map[string]any{"historyId": "999"})

	gmailMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/gmail/v1/users/me/profile" {
			_, _ = w.Write(profileResp)
			return
		}
		http.NotFound(w, r)
	}))
	defer gmailMock.Close()

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	q := queue.New(testutil.StartRedis(t))

	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("init+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	_ = oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok)

	sch := scheduler.New(scheduler.Config{
		Store: s, Queue: q, EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: gmailMock.URL,
	})

	if err := sch.PollOnce(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}

	cursor, _ := s.GetGmailSyncState(context.Background(), u.ID)
	if cursor != "999" {
		t.Fatalf("cursor after init: got %q, want 999", cursor)
	}
	depth, _ := q.Depth(context.Background(), "fetch")
	if depth != 0 {
		t.Fatalf("expected no jobs on init, got %d", depth)
	}
}
