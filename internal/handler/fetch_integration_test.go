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

func TestRenderHandler_ParsesMultipartAndSavesAttachments(t *testing.T) {
	const msgRaw = "From: alice@example.com\r\n" +
		"Subject: hi\r\n" +
		"Date: Fri, 18 Apr 2026 10:30:45 +0000\r\n" +
		"Content-Type: multipart/mixed; boundary=MIX\r\n\r\n" +
		"--MIX\r\n" +
		"Content-Type: text/html; charset=utf-8\r\n\r\n" +
		"<p>Hello</p>\r\n" +
		"--MIX\r\n" +
		"Content-Type: application/pdf; name=report.pdf\r\n" +
		"Content-Disposition: attachment; filename=report.pdf\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		"SGVsbG9QREY=\r\n" +
		"--MIX--\r\n"

	const fakePDF = "%PDF-1.7\nfake\n%%EOF"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(fakePDF))
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	jobID := uuid.New()

	mimeDir := filepath.Join(dataDir, "mime")
	if err := os.MkdirAll(mimeDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mimeDir, jobID.String()+".eml"), []byte(msgRaw), 0o600); err != nil {
		t.Fatal(err)
	}

	h := handler.NewRenderHandler(handler.RenderConfig{DataDir: dataDir, PDFEndpoint: srv.URL})

	job := store.Job{ID: jobID, Stage: "render"}
	next, err := h.Process(context.Background(), job)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if next != "upload" {
		t.Fatalf("next: %q", next)
	}

	pdf, err := os.ReadFile(filepath.Join(dataDir, "pdf", jobID.String()+".pdf"))
	if err != nil || string(pdf) != fakePDF {
		t.Fatalf("pdf: %v / %q", err, pdf)
	}

	metaB, err := os.ReadFile(filepath.Join(dataDir, "meta", jobID.String()+".json"))
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	var meta handler.RenderMeta
	if err := json.Unmarshal(metaB, &meta); err != nil {
		t.Fatalf("parse meta: %v", err)
	}
	if meta.Subject != "hi" || meta.FromEmail != "alice@example.com" {
		t.Fatalf("meta: %+v", meta)
	}
	if len(meta.AttachmentNames) != 1 || meta.AttachmentNames[0] != "report.pdf" {
		t.Fatalf("attachment names: %v", meta.AttachmentNames)
	}

	attach, err := os.ReadFile(filepath.Join(dataDir, "attachments", jobID.String(), "report.pdf"))
	if err != nil {
		t.Fatalf("attachment: %v", err)
	}
	if string(attach) != "HelloPDF" {
		t.Fatalf("attachment content: %q", attach)
	}
}

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

func TestUploadHandler_UploadsToDateTreeAndReturnsTerminal(t *testing.T) {
	uploadJSON, _ := json.Marshal(map[string]any{"id": "uploaded-pdf-id"})
	listJSON, _ := json.Marshal(map[string]any{"files": []map[string]any{}})
	createJSON, _ := json.Marshal(map[string]any{"id": "folder-id"})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/files" && r.Method == http.MethodGet:
			_, _ = w.Write(listJSON)
		case r.URL.Path == "/files" && r.Method == http.MethodPost:
			_, _ = w.Write(createJSON)
		case r.URL.Path == "/upload/drive/v3/files":
			_, _ = w.Write(uploadJSON)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	pool := testutil.StartPostgres(t)
	if err := store.Migrate(context.Background(), pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	u, _ := s.CreateUser(context.Background(), fmt.Sprintf("up+%d@example.com", time.Now().UnixNano()))
	key := newKey32()
	saveFakeToken(t, s, u.ID, key)

	redis := testutil.StartRedis(t)

	dataDir := t.TempDir()
	jobID := uuid.New()

	// Write meta.json
	metaDir := filepath.Join(dataDir, "meta")
	if err := os.MkdirAll(metaDir, 0o750); err != nil {
		t.Fatal(err)
	}
	meta := handler.RenderMeta{
		Subject:         "hi",
		FromEmail:       "alice@example.com",
		ReceivedAt:      time.Date(2026, 4, 17, 10, 30, 45, 0, time.UTC),
		AttachmentNames: []string{"report.pdf"},
	}
	metaB, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(metaDir, jobID.String()+".json"), metaB, 0o600); err != nil {
		t.Fatal(err)
	}

	// Write attachment file
	attachDir := filepath.Join(dataDir, "attachments", jobID.String())
	if err := os.MkdirAll(attachDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachDir, "report.pdf"), []byte("PDF-DATA-2"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Write email PDF
	pdfDir := filepath.Join(dataDir, "pdf")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pdfDir, jobID.String()+".pdf"), []byte("PDF-DATA"), 0o600); err != nil {
		t.Fatal(err)
	}

	h := handler.NewUploadHandler(handler.UploadConfig{
		Store:         s,
		EncryptionKey: key,
		OAuth2:        &oauth2.Config{ClientID: "x", ClientSecret: "y"},
		DriveEndpoint: srv.URL,
		Redis:         redis,
		DataDir:       dataDir,
		RootFolderID:  "root-folder-id",
	})

	job := store.Job{ID: jobID, UserID: &u.ID, Stage: "upload", CreatedAt: time.Date(2026, 4, 17, 0, 0, 0, 0, time.UTC)}
	next, err := h.Process(context.Background(), job)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if next != "" {
		t.Fatalf("next: %q, want '' (terminal)", next)
	}
}
