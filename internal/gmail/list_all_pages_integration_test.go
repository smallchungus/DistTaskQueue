//go:build integration

package gmail_test

import (
	"context"
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

func setupListAllPagesClient(t *testing.T, handler http.HandlerFunc) *gmail.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/gmail/v1/users/me/messages", handler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("lap+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	_ = oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok)

	c, err := gmail.New(context.Background(), gmail.Config{
		Store: s, UserID: u.ID, EncryptionKey: key,
		OAuth2:   &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		Endpoint: srv.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestListAllPages_DeliversPagesInOrder(t *testing.T) {
	page1 := map[string]any{
		"messages":      []map[string]any{{"id": "a1"}, {"id": "a2"}},
		"nextPageToken": "TOK",
	}
	page2 := map[string]any{"messages": []map[string]any{{"id": "a3"}}}

	c := setupListAllPagesClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		resp := page1
		if r.URL.Query().Get("pageToken") == "TOK" {
			resp = page2
		}
		b, _ := json.Marshal(resp)
		_, _ = w.Write(b)
	})

	var got [][]string
	err := c.ListAllPages(context.Background(), time.Time{}, time.Time{}, func(ids []string) error {
		got = append(got, append([]string(nil), ids...))
		return nil
	})
	if err != nil {
		t.Fatalf("list all pages: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("pages: %v, want 2 pages", got)
	}
	if len(got[0]) != 2 || got[0][0] != "a1" || got[0][1] != "a2" {
		t.Fatalf("page 1: %v, want [a1 a2]", got[0])
	}
	if len(got[1]) != 1 || got[1][0] != "a3" {
		t.Fatalf("page 2: %v, want [a3]", got[1])
	}
}

func TestListAllPages_QueryOmitsZeroTimes(t *testing.T) {
	tests := []struct {
		name   string
		since  time.Time
		before time.Time
		want   string
	}{
		{"both zero", time.Time{}, time.Time{}, ""},
		{"since only", time.Unix(1700000000, 0).UTC(), time.Time{}, "after:1700000000"},
		{"before only", time.Time{}, time.Unix(1800000000, 0).UTC(), "before:1800000000"},
		{"both set", time.Unix(1700000000, 0).UTC(), time.Unix(1800000000, 0).UTC(), "after:1700000000 before:1800000000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var capturedQuery string
			c := setupListAllPagesClient(t, func(w http.ResponseWriter, r *http.Request) {
				capturedQuery = r.URL.Query().Get("q")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"messages":[]}`))
			})
			if err := c.ListAllPages(context.Background(), tt.since, tt.before, func(ids []string) error { return nil }); err != nil {
				t.Fatalf("list all pages: %v", err)
			}
			if capturedQuery != tt.want {
				t.Fatalf("query: %q, want %q", capturedQuery, tt.want)
			}
		})
	}
}
