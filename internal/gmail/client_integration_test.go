//go:build integration

package gmail_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

type gmailMock struct {
	historyJSON string
	messageJSON string
	historyHits int
	messageHits int
}

func (m *gmailMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/history", func(w http.ResponseWriter, _ *http.Request) {
		m.historyHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.historyJSON))
	})
	mux.HandleFunc("/gmail/v1/users/me/messages/", func(w http.ResponseWriter, _ *http.Request) {
		m.messageHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.messageJSON))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func newKey32() []byte {
	k := make([]byte, 32)
	for i := range k {
		k[i] = byte(i)
	}
	return k
}

func setupClient(t *testing.T, m *gmailMock) (*gmail.Client, *store.Store) {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)

	u, err := s.CreateUser(context.Background(), fmt.Sprintf("test+%d@example.com", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}

	key := newKey32()
	tok := &oauth2.Token{
		AccessToken:  "fake-access",
		RefreshToken: "fake-refresh",
		Expiry:       time.Now().Add(1 * time.Hour),
	}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}

	srv := m.server(t)
	cfg := &oauth2.Config{ClientID: "x", ClientSecret: "y"}
	c, err := gmail.New(context.Background(), gmail.Config{
		Store:         s,
		UserID:        u.ID,
		EncryptionKey: key,
		OAuth2:        cfg,
		Endpoint:      srv.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c, s
}

func TestLatestMessageIDs_ReturnsInboxPrimaryAdds(t *testing.T) {
	histResp := map[string]any{
		"history": []map[string]any{
			{
				"id": "100",
				"messagesAdded": []map[string]any{
					{"message": map[string]any{"id": "m1", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
					{"message": map[string]any{"id": "m2", "labelIds": []string{"INBOX", "CATEGORY_PROMOTIONS"}}},
					{"message": map[string]any{"id": "m3", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
				},
			},
		},
		"historyId": "150",
	}
	b, _ := json.Marshal(histResp)

	c, _ := setupClient(t, &gmailMock{historyJSON: string(b)})

	ids, cursor, err := c.LatestMessageIDs(context.Background(), "1")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(ids) != 2 || ids[0] != "m1" || ids[1] != "m3" {
		t.Fatalf("ids: %v, want [m1 m3]", ids)
	}
	if cursor != "150" {
		t.Fatalf("cursor: %q, want 150", cursor)
	}
}

func TestLatestMessageIDs_IncludesLabelAddedForMovedToInbox(t *testing.T) {
	// This covers the "moved from Spam to Inbox" case — Gmail reports a
	// labelAdded event (not messageAdded) when a label is applied to an
	// existing message.
	histResp := map[string]any{
		"history": []map[string]any{
			{
				"id": "100",
				"labelsAdded": []map[string]any{
					{
						"message":  map[string]any{"id": "m4", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}},
						"labelIds": []string{"INBOX"},
					},
					{
						"message":  map[string]any{"id": "m5", "labelIds": []string{"INBOX", "CATEGORY_PROMOTIONS"}},
						"labelIds": []string{"INBOX"},
					},
				},
			},
		},
		"historyId": "175",
	}
	b, _ := json.Marshal(histResp)

	c, _ := setupClient(t, &gmailMock{historyJSON: string(b)})

	ids, cursor, err := c.LatestMessageIDs(context.Background(), "1")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(ids) != 1 || ids[0] != "m4" {
		t.Fatalf("ids: %v, want [m4] (m5 is CATEGORY_PROMOTIONS, filtered)", ids)
	}
	if cursor != "175" {
		t.Fatalf("cursor: %q, want 175", cursor)
	}
}

func TestLatestMessageIDs_DeduplicatesAcrossAddedAndLabelAdded(t *testing.T) {
	// A single email can appear in both messagesAdded (when first landed in
	// INBOX) and labelsAdded (when e.g. a second label got added). It must
	// only be enqueued once.
	histResp := map[string]any{
		"history": []map[string]any{
			{
				"id": "100",
				"messagesAdded": []map[string]any{
					{"message": map[string]any{"id": "dup-1", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL"}}},
				},
				"labelsAdded": []map[string]any{
					{
						"message":  map[string]any{"id": "dup-1", "labelIds": []string{"INBOX", "CATEGORY_PERSONAL", "IMPORTANT"}},
						"labelIds": []string{"IMPORTANT"},
					},
				},
			},
		},
		"historyId": "180",
	}
	b, _ := json.Marshal(histResp)

	c, _ := setupClient(t, &gmailMock{historyJSON: string(b)})

	ids, _, err := c.LatestMessageIDs(context.Background(), "1")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(ids) != 1 || ids[0] != "dup-1" {
		t.Fatalf("ids: %v, want [dup-1] (deduplicated)", ids)
	}
}

func TestLatestMessageIDs_EmptyHistory(t *testing.T) {
	histResp := map[string]any{"historyId": "200"}
	b, _ := json.Marshal(histResp)

	c, _ := setupClient(t, &gmailMock{historyJSON: string(b)})

	ids, cursor, err := c.LatestMessageIDs(context.Background(), "1")
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids: %v, want empty", ids)
	}
	if cursor != "200" {
		t.Fatalf("cursor: %q, want 200", cursor)
	}
}

func TestListRecent_ReturnsMessageIDs(t *testing.T) {
	// messages.list returns up to maxResults IDs filtered by labelIds + q.
	// Backfill uses this to catch anything the History API missed.
	listResp := map[string]any{
		"messages": []map[string]any{
			{"id": "a1"}, {"id": "a2"}, {"id": "a3"},
		},
		"resultSizeEstimate": 3,
	}
	b, _ := json.Marshal(listResp)

	mux := http.NewServeMux()
	var capturedQuery string
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		capturedQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("lr+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	_ = oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok)

	c, _ := gmail.New(context.Background(), gmail.Config{
		Store: s, UserID: u.ID, EncryptionKey: key,
		OAuth2:   &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		Endpoint: srv.URL,
	})

	since := time.Unix(1700000000, 0).UTC()
	ids, err := c.ListRecent(context.Background(), since)
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(ids) != 3 || ids[0] != "a1" || ids[1] != "a2" || ids[2] != "a3" {
		t.Fatalf("ids: %v, want [a1 a2 a3]", ids)
	}
	// Query must filter by INBOX + CATEGORY_PERSONAL + after:<unix-ts>.
	if !containsAll(capturedQuery, "INBOX", "CATEGORY_PERSONAL", "after%3A1700000000") {
		t.Fatalf("query did not include required filters: %q", capturedQuery)
	}
}

func TestListRecent_PaginatesAcrossPages(t *testing.T) {
	page1 := map[string]any{
		"messages":      []map[string]any{{"id": "p1"}, {"id": "p2"}},
		"nextPageToken": "TOK",
	}
	page2 := map[string]any{
		"messages": []map[string]any{{"id": "p3"}},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("pageToken") == "TOK" {
			b, _ := json.Marshal(page2)
			_, _ = w.Write(b)
			return
		}
		b, _ := json.Marshal(page1)
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("pg+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	_ = oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok)

	c, _ := gmail.New(context.Background(), gmail.Config{
		Store: s, UserID: u.ID, EncryptionKey: key,
		OAuth2:   &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		Endpoint: srv.URL,
	})

	ids, err := c.ListRecent(context.Background(), time.Unix(1700000000, 0).UTC())
	if err != nil {
		t.Fatalf("list recent: %v", err)
	}
	if len(ids) != 3 || ids[0] != "p1" || ids[1] != "p2" || ids[2] != "p3" {
		t.Fatalf("ids: %v, want [p1 p2 p3]", ids)
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func TestFetchMessage_DecodesRawBase64(t *testing.T) {
	rawMime := "From: alice@example.com\r\nSubject: hi\r\n\r\nbody"
	rawB64 := base64URL(rawMime)
	msgResp := map[string]any{"raw": rawB64}
	b, _ := json.Marshal(msgResp)

	c, _ := setupClient(t, &gmailMock{messageJSON: string(b)})

	got, err := c.FetchMessage(context.Background(), "m1")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if string(got) != rawMime {
		t.Fatalf("got %q, want %q", got, rawMime)
	}
}

func base64URL(s string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
}
