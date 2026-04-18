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
