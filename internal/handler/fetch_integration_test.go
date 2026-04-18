//go:build integration

package handler_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/handler"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
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

func saveFakeToken(t *testing.T, s *store.Store, uid uuid.UUID, key []byte) {
	t.Helper()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, uid, key, "google", tok); err != nil {
		t.Fatal(err)
	}
}

func encodeURL(s string) string {
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))
}

func TestFetchHandler_WritesMimeAndReturnsRender(t *testing.T) {
	const rawMime = "From: alice@example.com\r\nSubject: hi\r\n\r\nbody"

	encB64 := encodeURL(rawMime)
	msgJSON, _ := json.Marshal(map[string]any{"raw": encB64})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(msgJSON)
	}))
	defer srv.Close()

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("u+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	saveFakeToken(t, s, u.ID, key)

	dataDir := t.TempDir()

	h := handler.NewFetchHandler(handler.FetchConfig{
		Store:         s,
		EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		GmailEndpoint: srv.URL,
		DataDir:       dataDir,
	})

	gmailMsgID := "m1"
	job, _ := s.EnqueueJob(context.Background(), store.NewJob{
		Stage:          "fetch",
		UserID:         &u.ID,
		GmailMessageID: &gmailMsgID,
	})
	_ = s.ClaimJob(context.Background(), job.ID, "w1")

	job, _ = s.GetJob(context.Background(), job.ID)
	next, err := h.Process(context.Background(), job)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if next != "render" {
		t.Fatalf("next: %q, want render", next)
	}

	written, err := os.ReadFile(filepath.Join(dataDir, "mime", job.ID.String()+".eml"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(written) != rawMime {
		t.Fatalf("mime: %q, want %q", written, rawMime)
	}
}
