# Phase 3E — Stage Handlers (fetch → render → upload)

> Use `superpowers:subagent-driven-development`. `- [ ]` checkboxes track steps.

**Goal:** Ship the three real handlers that turn a Gmail message ID into a PDF on Drive. Plus the supporting refactors: Job model gains user/email columns, Handler interface gains a `nextStage` return, Worker.ProcessOne advances jobs through stages instead of marking each terminal. cmd/worker picks the right handler from `--stage`.

**Out of scope (deferred):** attachment preservation, scheduler binary, manual OAuth setup. Those land in Phase 3F.

**Architecture:**

```
internal/store/
  types.go           Job adds UserID *uuid.UUID, GmailMessageID *string, IsSynthetic bool
  store.go           all 7 queries updated to include the new columns
  store.go           NEW: AdvanceJob(jobID, nextStage)
internal/worker/
  handler.go         Handler interface returns (nextStage string, err error); NoopHandler returns ("", nil)
  worker.go          ProcessOne uses nextStage: AdvanceJob+Push if non-empty, else MarkDone
internal/handler/
  fetch.go           FetchHandler — gmail.FetchMessage → /data/mime/<job_id>.eml → "render"
  render.go          RenderHandler — read MIME → render HTML body via pdf.Client → /data/pdf/<job_id>.pdf → "upload"
  upload.go          UploadHandler — drive folder ensure (YYYY/MM/DD) → upload PDF → "" (terminal)
  paths.go           DateTreeFolders(t) []string ; EmailFileName(t, jobID) string
  *_integration_test.go    httptest mocks + tmpdir for filesystem
cmd/worker/main.go   switch on --stage to construct the right handler
```

**Branch:** `phase-3e-handlers`.

---

## Section A — Job model + AdvanceJob (TDD)

### Task A1: Branch

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3e-handlers
```

### Task A2: Extend Job + NewJob types

Modify `internal/store/types.go` — add three fields to `Job`:

```go
type Job struct {
	ID              uuid.UUID
	UserID          *uuid.UUID    // nullable (synthetic jobs have no user)
	GmailMessageID  *string       // nullable
	Stage           string
	Status          JobStatus
	Payload         json.RawMessage
	IsSynthetic     bool
	WorkerID        *string
	Attempts        int
	MaxAttempts     int
	LastError       *string
	NextRunAt       time.Time
	ClaimedAt       *time.Time
	CompletedAt     *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}
```

And `NewJob`:

```go
type NewJob struct {
	Stage          string
	Payload        json.RawMessage
	UserID         *uuid.UUID
	GmailMessageID *string
	IsSynthetic    bool
}
```

### Task A3: Update store CRUD queries

Modify `internal/store/store.go`:

- `EnqueueJob`: INSERT now writes `user_id, gmail_message_id, is_synthetic`. RETURNING list adds them.
- `GetJob`: SELECT list adds them.
- `MarkDone`, `MarkFailed`, `ClaimJob`: no schema change to writes; but the RETURNING/SELECT in their callers gets the new fields when `GetJob` is the next call. (ClaimJob doesn't return a Job; only updates. Keep as-is.)
- `scanJobs` helper (used by ListRunningJobs / ListReadyRetryJobs): add the new fields to the Scan call.

The exact column list for SELECT: `id, user_id, gmail_message_id, stage, status, payload, is_synthetic, worker_id, attempts, max_attempts, last_error, next_run_at, claimed_at, completed_at, created_at, updated_at`.

The Scan order in `EnqueueJob`, `GetJob`, and `scanJobs` must match that column list exactly. Pointers for nullable: `&j.UserID`, `&j.GmailMessageID` (both `*uuid.UUID` / `*string` — pgx scans into a `*sql.Null*`-like indirection; pgx/v5 supports scanning NULL into pointer-to-T directly).

INSERT in `EnqueueJob`:

```sql
INSERT INTO pipeline_jobs (id, user_id, gmail_message_id, stage, status, payload, is_synthetic)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, user_id, gmail_message_id, stage, status, payload, is_synthetic,
          worker_id, attempts, max_attempts, last_error, next_run_at,
          claimed_at, completed_at, created_at, updated_at
```

Pass `nj.UserID, nj.GmailMessageID, ..., nj.IsSynthetic` into the args.

### Task A4: Verify existing tests still pass

```bash
go test -tags=integration ./internal/store/...
```

Expected: 17 tests pass. The existing tests don't set the new fields; they should still work because EnqueueJob writes NULL/false defaults.

Commit: `refactor(store): extend Job model with user_id, gmail_message_id, is_synthetic`

### Task A5: Add AdvanceJob method

Failing test, append to `internal/store/store_integration_test.go`:

```go
func TestAdvanceJob_TransitionsToNextStage(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j, _ := s.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	_ = s.ClaimJob(ctx, j.ID, "w1")

	if err := s.AdvanceJob(ctx, j.ID, "render"); err != nil {
		t.Fatalf("advance: %v", err)
	}

	got, _ := s.GetJob(ctx, j.ID)
	if got.Stage != "render" {
		t.Fatalf("stage: %s, want render", got.Stage)
	}
	if got.Status != store.StatusQueued {
		t.Fatalf("status: %s, want queued", got.Status)
	}
	if got.WorkerID != nil {
		t.Fatalf("worker_id: %v, want nil", got.WorkerID)
	}
}
```

Run — expect undefined `s.AdvanceJob`. Capture.

Implement, append to `internal/store/store.go`:

```go
func (s *Store) AdvanceJob(ctx context.Context, id uuid.UUID, nextStage string) error {
	const q = `
		UPDATE pipeline_jobs
		SET stage = $1, status = $2, worker_id = NULL, claimed_at = NULL, updated_at = now()
		WHERE id = $3 AND status = $4`

	tag, err := s.pool.Exec(ctx, q, nextStage, StatusQueued, id, StatusRunning)
	if err != nil {
		return fmt.Errorf("advance: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrJobNotFound
	}
	return nil
}
```

Run — expect 18 tests pass.

Commit: `feat(store): add AdvanceJob to transition a running job to next stage queued`

---

## Section B — Handler interface refactor

### Task B1: Failing tests + interface change

Update `internal/worker/handler.go`:

```go
type Handler interface {
	Process(ctx context.Context, job store.Job) (nextStage string, err error)
}

type NoopHandler struct{}

func (NoopHandler) Process(ctx context.Context, job store.Job) (string, error) {
	var p struct {
		SleepMs int `json:"sleepMs"`
	}
	if len(job.Payload) > 0 {
		_ = json.Unmarshal(job.Payload, &p)
	}
	if p.SleepMs <= 0 {
		return "", nil
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Duration(p.SleepMs) * time.Millisecond):
		return "", nil
	}
}
```

Update `internal/worker/handler_test.go`: change `err := h.Process(...)` to `_, err := h.Process(...)` (two-value).

### Task B2: Update Worker.ProcessOne

Modify the handler call section in `internal/worker/worker.go`:

```go
	hbCtx, hbCancel := context.WithCancel(ctx)
	go w.heartbeatLoop(hbCtx)

	nextStage, handlerErr := w.cfg.Handler.Process(ctx, job)

	hbCancel()

	if handlerErr != nil {
		nextRun := time.Now().Add(Compute(job.Attempts + 1))
		if err := w.cfg.Store.MarkFailed(ctx, jobID, handlerErr.Error(), nextRun); err != nil {
			return true, fmt.Errorf("mark failed: %w", err)
		}
		slog.Info("job failed, will retry", "job_id", jobID, "attempts", job.Attempts+1, "next_run", nextRun)
		return true, nil
	}

	if nextStage != "" {
		if err := w.cfg.Store.AdvanceJob(ctx, jobID, nextStage); err != nil {
			return true, fmt.Errorf("advance: %w", err)
		}
		if err := w.cfg.Queue.Push(ctx, nextStage, jobID.String()); err != nil {
			return true, fmt.Errorf("push next stage: %w", err)
		}
		slog.Info("job advanced", "job_id", jobID, "from", job.Stage, "to", nextStage)
		return true, nil
	}

	if err := w.cfg.Store.MarkDone(ctx, jobID); err != nil {
		return true, fmt.Errorf("mark done: %w", err)
	}
	return true, nil
```

### Task B3: Update worker integration tests

In `internal/worker/worker_integration_test.go`:
- `errHandler.Process` signature: change `error` → `(string, error)`. Implementation returns `"", &handlerErr{"boom"}`.
- The existing tests expect MarkDone (no advance), which still works because NoopHandler returns "" + nil.

Add a new test:

```go
type advancingHandler struct{ next string }

func (h advancingHandler) Process(ctx context.Context, job store.Job) (string, error) {
	return h.next, nil
}

func TestProcessOne_AdvancesToNextStage(t *testing.T) {
	h := setupHarness(t, advancingHandler{next: "render"})
	ctx := context.Background()
	job, _ := h.store.EnqueueJob(ctx, store.NewJob{Stage: "fetch"})
	_ = h.queue.Push(ctx, "fetch", job.ID.String())

	didWork, err := h.w.ProcessOne(ctx)
	if err != nil {
		t.Fatalf("ProcessOne: %v", err)
	}
	if !didWork {
		t.Fatal("expected didWork=true")
	}

	got, _ := h.store.GetJob(ctx, job.ID)
	if got.Stage != "render" || got.Status != store.StatusQueued {
		t.Fatalf("stage/status: %s/%s, want render/queued", got.Stage, got.Status)
	}

	// Verify it was pushed to render queue
	popped, err := h.queue.BlockingPop(ctx, "render", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("expected pushed to render: %v", err)
	}
	if popped != job.ID.String() {
		t.Fatalf("popped %q, want %q", popped, job.ID.String())
	}
}
```

You may need `setupHarness` to use stage `"fetch"` instead of `"test"` for this test, OR construct a fresh harness. Adjust as needed.

Run — expect all tests pass (the existing ones continue, plus the new one).

Commit: `refactor(worker): handler returns nextStage; ProcessOne advances or marks done`

---

## Section C — Fetch handler (TDD)

### Task C1: Failing test

Create `internal/handler/fetch_integration_test.go`:

```go
//go:build integration

package handler_test

import (
	"context"
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

func TestFetchHandler_WritesMimeAndReturnsRender(t *testing.T) {
	const rawMime = "From: alice@example.com\r\nSubject: hi\r\n\r\nbody"
	rawB64 := func(s string) string {
		// gmail returns base64url no-padding
		out := make([]byte, 0, len(s)*2)
		for _, b := range []byte(s) {
			out = append(out, b)
		}
		return ""
	}
	_ = rawB64
	// gmail mock returns JSON {"raw": "<base64url>"}
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

// encodeURL helper: base64url no-padding
func encodeURL(s string) string {
	import_ := func() string { return "" }
	_ = import_
	// Inline base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString
	return base64URLEncode(s)
}
```

Add the `encodeURL` / `base64URLEncode` helper using the standard `encoding/base64` package (`base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString([]byte(s))`).

Run `go test -tags=integration ./internal/handler/...` — expect undefined `handler.NewFetchHandler`. Capture.

### Task C2: Implement FetchHandler

Create `internal/handler/fetch.go`:

```go
package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type FetchConfig struct {
	Store         *store.Store
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	GmailEndpoint string // empty -> Google's default
	DataDir       string // base dir; mime files go to <DataDir>/mime/<jobID>.eml
}

type FetchHandler struct {
	cfg FetchConfig
}

func NewFetchHandler(cfg FetchConfig) *FetchHandler {
	return &FetchHandler{cfg: cfg}
}

func (h *FetchHandler) Process(ctx context.Context, job store.Job) (string, error) {
	if job.UserID == nil {
		return "", errors.New("fetch: job missing user_id")
	}
	if job.GmailMessageID == nil {
		return "", errors.New("fetch: job missing gmail_message_id")
	}

	client, err := gmail.New(ctx, gmail.Config{
		Store:         h.cfg.Store,
		UserID:        *job.UserID,
		EncryptionKey: h.cfg.EncryptionKey,
		OAuth2:        h.cfg.OAuth2,
		Endpoint:      h.cfg.GmailEndpoint,
	})
	if err != nil {
		return "", fmt.Errorf("gmail client: %w", err)
	}

	raw, err := client.FetchMessage(ctx, *job.GmailMessageID)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}

	dir := filepath.Join(h.cfg.DataDir, "mime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	path := filepath.Join(dir, job.ID.String()+".eml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write mime: %w", err)
	}

	return "render", nil
}
```

Run integration tests — expect 1 pass.

Commit: `feat(handler): add FetchHandler — gmail message -> /data/mime/<id>.eml`

---

## Section D — Render handler (TDD)

### Task D1: Failing test

Append to `internal/handler/fetch_integration_test.go` (or new file `render_integration_test.go`):

```go
func TestRenderHandler_WritesPDFAndReturnsUpload(t *testing.T) {
	const fakeMime = "From: a@b.com\r\nSubject: hi\r\nContent-Type: text/html\r\n\r\n<h1>Hello</h1>"
	const fakePDF = "%PDF-1.7\nfake\n%%EOF"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(fakePDF))
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	jobID := uuid.New()

	mimeDir := filepath.Join(dataDir, "mime")
	if err := os.MkdirAll(mimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mimeDir, jobID.String()+".eml"), []byte(fakeMime), 0o600); err != nil {
		t.Fatal(err)
	}

	h := handler.NewRenderHandler(handler.RenderConfig{
		DataDir:     dataDir,
		PDFEndpoint: srv.URL,
	})

	job := store.Job{ID: jobID, Stage: "render"}
	next, err := h.Process(context.Background(), job)
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if next != "upload" {
		t.Fatalf("next: %q, want upload", next)
	}

	written, err := os.ReadFile(filepath.Join(dataDir, "pdf", jobID.String()+".pdf"))
	if err != nil {
		t.Fatalf("read pdf: %v", err)
	}
	if string(written) != fakePDF {
		t.Fatalf("pdf: got %q, want %q", written, fakePDF)
	}
}
```

Run — expect undefined `handler.NewRenderHandler`. Capture.

### Task D2: Implement RenderHandler

Create `internal/handler/render.go`:

```go
package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"

	"github.com/smallchungus/disttaskqueue/internal/pdf"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type RenderConfig struct {
	DataDir     string
	PDFEndpoint string
}

type RenderHandler struct {
	cfg    RenderConfig
	client *pdf.Client
}

func NewRenderHandler(cfg RenderConfig) *RenderHandler {
	return &RenderHandler{cfg: cfg, client: pdf.New(cfg.PDFEndpoint)}
}

func (h *RenderHandler) Process(ctx context.Context, job store.Job) (string, error) {
	mimePath := filepath.Join(h.cfg.DataDir, "mime", job.ID.String()+".eml")
	rawMime, err := os.ReadFile(mimePath)
	if err != nil {
		return "", fmt.Errorf("read mime: %w", err)
	}

	html, err := htmlFromMime(rawMime)
	if err != nil {
		return "", fmt.Errorf("parse mime: %w", err)
	}

	pdfBytes, err := h.client.RenderHTML(ctx, html)
	if err != nil {
		return "", fmt.Errorf("render: %w", err)
	}

	pdfDir := filepath.Join(h.cfg.DataDir, "pdf")
	if err := os.MkdirAll(pdfDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	pdfPath := filepath.Join(pdfDir, job.ID.String()+".pdf")
	if err := os.WriteFile(pdfPath, pdfBytes, 0o600); err != nil {
		return "", fmt.Errorf("write pdf: %w", err)
	}

	return "upload", nil
}

// htmlFromMime extracts an HTML representation of the message body. For v1 we
// simply read the body of the parsed message (single-part); multipart/alternative
// and attachment handling lands in a follow-up phase.
func htmlFromMime(raw []byte) ([]byte, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("read message: %w", err)
	}
	subject := msg.Header.Get("Subject")
	from := msg.Header.Get("From")
	date := msg.Header.Get("Date")

	body := &bytes.Buffer{}
	if _, err := body.ReadFrom(msg.Body); err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if subject == "" && from == "" && body.Len() == 0 {
		return nil, errors.New("empty message")
	}

	out := &bytes.Buffer{}
	fmt.Fprintf(out, "<!doctype html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body>", htmlEscape(subject))
	fmt.Fprintf(out, "<div style=\"font-family:sans-serif;border-bottom:1px solid #ccc;padding-bottom:8px;margin-bottom:12px\">")
	fmt.Fprintf(out, "<div><strong>From:</strong> %s</div>", htmlEscape(from))
	fmt.Fprintf(out, "<div><strong>Date:</strong> %s</div>", htmlEscape(date))
	fmt.Fprintf(out, "<div><strong>Subject:</strong> %s</div>", htmlEscape(subject))
	fmt.Fprintf(out, "</div>")
	contentType := msg.Header.Get("Content-Type")
	if contentType == "" || contentType[:9] == "text/html" {
		out.Write(body.Bytes())
	} else {
		fmt.Fprintf(out, "<pre>%s</pre>", htmlEscape(body.String()))
	}
	out.WriteString("</body></html>")
	return out.Bytes(), nil
}

func htmlEscape(s string) string {
	r := bytes.NewBuffer(nil)
	for _, c := range []byte(s) {
		switch c {
		case '<':
			r.WriteString("&lt;")
		case '>':
			r.WriteString("&gt;")
		case '&':
			r.WriteString("&amp;")
		case '"':
			r.WriteString("&quot;")
		default:
			r.WriteByte(c)
		}
	}
	return r.String()
}
```

Run — expect 2 handler integration tests pass.

Commit: `feat(handler): add RenderHandler — MIME body -> Gotenberg -> /data/pdf/<id>.pdf`

---

## Section E — Upload handler + paths (TDD)

### Task E1: paths.go (pure unit)

Create `internal/handler/paths.go`:

```go
package handler

import (
	"fmt"
	"time"
)

// DateTreeFolders returns the year/month/day folder names for the given time.
// e.g. ["2026", "04", "17"]
func DateTreeFolders(t time.Time) []string {
	t = t.UTC()
	return []string{
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", int(t.Month())),
		fmt.Sprintf("%02d", t.Day()),
	}
}
```

Test in `internal/handler/paths_test.go`:

```go
package handler

import (
	"reflect"
	"testing"
	"time"
)

func TestDateTreeFolders(t *testing.T) {
	got := DateTreeFolders(time.Date(2026, 4, 17, 10, 30, 0, 0, time.UTC))
	want := []string{"2026", "04", "17"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
```

Run unit tests — expect pass.

Commit: `feat(handler): add DateTreeFolders for date-tree Drive layout`

### Task E2: UploadHandler

Failing test, append to handler integration tests:

```go
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
```

Add imports: `_ "encoding/json"`, etc. (already present).

Run — expect undefined `handler.NewUploadHandler`.

Implement `internal/handler/upload.go`:

```go
package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/drive"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type UploadConfig struct {
	Store         *store.Store
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	DriveEndpoint string
	Redis         *goredis.Client
	DataDir       string
	RootFolderID  string
}

type UploadHandler struct {
	cfg   UploadConfig
	cache *drive.FolderCache
}

func NewUploadHandler(cfg UploadConfig) *UploadHandler {
	return &UploadHandler{cfg: cfg, cache: drive.NewFolderCache(cfg.Redis)}
}

func (h *UploadHandler) Process(ctx context.Context, job store.Job) (string, error) {
	if job.UserID == nil {
		return "", errors.New("upload: job missing user_id")
	}

	client, err := drive.New(ctx, drive.Config{
		Store:         h.cfg.Store,
		UserID:        *job.UserID,
		EncryptionKey: h.cfg.EncryptionKey,
		OAuth2:        h.cfg.OAuth2,
		Endpoint:      h.cfg.DriveEndpoint,
	})
	if err != nil {
		return "", fmt.Errorf("drive client: %w", err)
	}

	folders := DateTreeFolders(job.CreatedAt)
	parent := h.cfg.RootFolderID
	pathSoFar := ""
	for _, f := range folders {
		if pathSoFar == "" {
			pathSoFar = f
		} else {
			pathSoFar = path.Join(pathSoFar, f)
		}

		if cached, ok, err := h.cache.Get(ctx, *job.UserID, pathSoFar); err == nil && ok {
			parent = cached
			continue
		}

		folderID, err := client.EnsureFolder(ctx, parent, f)
		if err != nil {
			return "", fmt.Errorf("ensure folder %s: %w", f, err)
		}
		_ = h.cache.Set(ctx, *job.UserID, pathSoFar, folderID, 24*time.Hour)
		parent = folderID
	}

	pdfBytes, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "pdf", job.ID.String()+".pdf"))
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}

	name := job.ID.String() + ".pdf"
	if _, err := client.Upload(ctx, parent, name, "application/pdf", pdfBytes); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	return "", nil // terminal
}
```

Run integration tests — expect 3 handler integration tests pass.

Commit: `feat(handler): add UploadHandler with date-tree folders and FolderCache`

---

## Section F — cmd/worker selects handler by --stage

Modify `cmd/worker/main.go` to switch on `*stage`:

```go
import (
	// ... existing imports
	"encoding/base64"
	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/smallchungus/disttaskqueue/internal/handler"
)

// after parsing flag and setting up pool/redis, replace the worker.NoopHandler{} with:

var h worker.Handler
switch *stage {
case "test":
	h = worker.NoopHandler{}
case "fetch":
	cfg, key := loadGoogleCfgAndKey()
	h = handler.NewFetchHandler(handler.FetchConfig{
		Store:         store.New(pool),
		EncryptionKey: key,
		OAuth2:        cfg,
		DataDir:       envOr("DATA_DIR", "/data"),
	})
case "render":
	h = handler.NewRenderHandler(handler.RenderConfig{
		DataDir:     envOr("DATA_DIR", "/data"),
		PDFEndpoint: envOr("GOTENBERG_URL", "http://gotenberg:3000"),
	})
case "upload":
	cfg, key := loadGoogleCfgAndKey()
	h = handler.NewUploadHandler(handler.UploadConfig{
		Store:         store.New(pool),
		EncryptionKey: key,
		OAuth2:        cfg,
		Redis:         redis,
		DataDir:       envOr("DATA_DIR", "/data"),
		RootFolderID:  envOr("DRIVE_ROOT_FOLDER_ID", ""),
	})
default:
	slog.Error("unknown stage", "stage", *stage)
	os.Exit(2)
}

// then pass `h` to worker.Config{Handler: h, ...}
```

And add the helper:

```go
func loadGoogleCfgAndKey() (*oauth2.Config, []byte) {
	clientID := mustEnv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := mustEnv("GOOGLE_OAUTH_CLIENT_SECRET")
	keyB64 := mustEnv("TOKEN_ENCRYPTION_KEY")
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		slog.Error("decode TOKEN_ENCRYPTION_KEY", "err", err)
		os.Exit(1)
	}
	if len(key) != 32 {
		slog.Error("TOKEN_ENCRYPTION_KEY must decode to 32 bytes", "got", len(key))
		os.Exit(1)
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
	}, key
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		slog.Error("missing env var", "key", k)
		os.Exit(1)
	}
	return v
}
```

Verify build:

```bash
go build ./cmd/worker
./worker --stage=test &  # using local PG/Redis from docker compose
```

(The fetch/render/upload stages require GOOGLE_OAUTH_CLIENT_ID etc. — they'll fail-fast if not set. That's fine; test stage works with NoopHandler regardless.)

Smoke test using `--stage=test` only (other stages need real OAuth credentials).

Commit: `feat(worker): wire fetch/render/upload handlers based on --stage flag`

---

## Section G — Local sweep + PR + tag

```bash
make lint && make test-unit && make test-integration && make k8s-validate && make loadtest
```

All exit 0.

```bash
git push -u origin phase-3e-handlers

gh pr create --base main --head phase-3e-handlers \
  --title "feat(handler): Phase 3E — Stage handlers (fetch/render/upload) + Job/Handler refactor" \
  --body "Phase 3E: the real Gmail->PDF->Drive pipeline.

- Job model gains user_id, gmail_message_id, is_synthetic columns (already in schema from 3A; now read/written by Go code).
- New store.AdvanceJob(jobID, nextStage) that transitions a running job to the next stage queued.
- Handler interface returns (nextStage, err). Worker.ProcessOne advances or marks done accordingly.
- Three stage handlers in internal/handler:
  - FetchHandler: gmail.FetchMessage -> /data/mime/<id>.eml -> 'render'
  - RenderHandler: parse MIME, build HTML wrapper around body, gotenberg -> /data/pdf/<id>.pdf -> 'upload'
  - UploadHandler: drive.EnsureFolder for YYYY/MM/DD (cached in Redis), drive.Upload PDF -> '' (terminal)
- cmd/worker switches on --stage to pick the handler.

Out of scope (Phase 3F): scheduler binary, manual OAuth setup helper, attachment preservation."

PR=$(gh pr view --json number -q .number)
gh pr merge $PR --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3e -m "Phase 3E: Stage handlers (fetch/render/upload)"
git push origin phase-3e
gh release create phase-3e --title "Phase 3E: Stage Handlers" \
  --notes "The real Gmail->PDF->Drive pipeline. Three handlers (fetch/render/upload), Job model gains user/email columns, Handler interface gains nextStage return, Worker.ProcessOne advances jobs through stages."
git push origin --delete phase-3e-handlers
git branch -D phase-3e-handlers
```
