//go:build integration

package drive_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/drive"
	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

type driveMock struct {
	listResp   string
	createResp string
	uploadResp string
	listHits   int
	createHits int
	uploadHits int
	lastUpload []byte
}

func (m *driveMock) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/files", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			m.listHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.listResp))
		case http.MethodPost:
			m.createHits++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(m.createResp))
		}
	})
	mux.HandleFunc("/upload/drive/v3/files", func(w http.ResponseWriter, r *http.Request) {
		m.uploadHits++
		body, _ := io.ReadAll(r.Body)
		m.lastUpload = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(m.uploadResp))
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

func setupClient(t *testing.T, m *driveMock) *drive.Client {
	t.Helper()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("drive+%d@example.com", time.Now().UnixNano()))

	key := newKey32()
	tok := &oauth2.Token{AccessToken: "x", RefreshToken: "y", Expiry: time.Now().Add(time.Hour)}
	if err := oauth.SaveToken(context.Background(), s, u.ID, key, "google", tok); err != nil {
		t.Fatal(err)
	}

	srv := m.server(t)
	cfg := &oauth2.Config{ClientID: "x", ClientSecret: "y"}
	c, err := drive.New(context.Background(), drive.Config{
		Store:         s,
		UserID:        u.ID,
		EncryptionKey: key,
		OAuth2:        cfg,
		Endpoint:      srv.URL,
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	return c
}

func TestEnsureFolder_ReturnsExistingFolderID(t *testing.T) {
	listJSON, _ := json.Marshal(map[string]any{
		"files": []map[string]any{{"id": "existing-folder-id", "name": "myfolder"}},
	})
	c := setupClient(t, &driveMock{listResp: string(listJSON)})

	id, err := c.EnsureFolder(context.Background(), "parent-id", "myfolder")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "existing-folder-id" {
		t.Fatalf("got %q, want existing-folder-id", id)
	}
}

func TestEnsureFolder_CreatesIfMissing(t *testing.T) {
	listJSON, _ := json.Marshal(map[string]any{"files": []map[string]any{}})
	createJSON, _ := json.Marshal(map[string]any{"id": "new-folder-id", "name": "newfolder"})
	c := setupClient(t, &driveMock{listResp: string(listJSON), createResp: string(createJSON)})

	id, err := c.EnsureFolder(context.Background(), "parent-id", "newfolder")
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if id != "new-folder-id" {
		t.Fatalf("got %q, want new-folder-id", id)
	}
}

func TestUpload_PostsContent(t *testing.T) {
	uploadJSON, _ := json.Marshal(map[string]any{"id": "uploaded-file-id"})
	m := &driveMock{uploadResp: string(uploadJSON)}
	c := setupClient(t, m)

	id, err := c.Upload(context.Background(), "parent-id", "report.pdf", "application/pdf", []byte("PDF-DATA"))
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if id != "uploaded-file-id" {
		t.Fatalf("got %q, want uploaded-file-id", id)
	}
	if !strings.Contains(string(m.lastUpload), "PDF-DATA") {
		t.Fatalf("upload body did not contain PDF-DATA")
	}
}
