# Phase 3G — Proper MIME parsing, attachments, folder-per-email

> Use `superpowers:subagent-driven-development`. `- [ ]` checkboxes track steps.

**Goal:** Make the output actually look like cloudhq's. Three concrete upgrades:

1. **Multipart MIME parsing** — walk the MIME tree, pick the HTML body part (fall back to text/plain), extract attachments separately. Current code dumps raw multipart into the PDF, which is unreadable for any modern email.
2. **Attachments preserved** — every attachment uploaded alongside the PDF.
3. **Folder per email + nice filenames** — each email gets its own folder named `<YYYY-MM-DD>_<HHMMSS>_<subject-slug>_<sender>`. Inside: `email.pdf` + original attachment filenames (sanitized).

Final Drive layout:

```
<root>/
  2026/
    04/
      18/
        2026-04-18_103045_re-project-update_alice/
          email.pdf
          report.docx
          diagram.png
```

**Architecture:**

```
internal/handler/
  mime.go          NEW: parseMessage, walkPart, decodePart, Attachment, ParsedMessage
  mime_test.go     NEW: pure unit tests against fixture MIME strings
  paths.go         + EmailFolderName, SubjectSlug, FromSlug, SanitizeFilename
  paths_test.go    + tests for those
  render.go        UPDATED: uses parseMessage, writes attachments to disk, writes meta.json
  upload.go        UPDATED: reads meta.json, creates email folder, uploads pdf + attachments
  render_integration_test.go   UPDATED/NEW with multipart fixture
  upload_integration_test.go   UPDATED/NEW
```

Cross-stage state lives on the `<DataDir>` volume:

```
<DataDir>/mime/<jobID>.eml               written by fetch, read by render
<DataDir>/pdf/<jobID>.pdf                written by render, read by upload
<DataDir>/attachments/<jobID>/<name>     written by render, read by upload (0..N files)
<DataDir>/meta/<jobID>.json              written by render, read by upload
```

`meta.json` shape:

```json
{
  "subject": "Re: project update",
  "from_email": "alice@example.com",
  "received_at": "2026-04-18T10:30:45Z",
  "attachment_names": ["report.docx", "diagram.png"]
}
```

**Branch:** `phase-3g-mime-attachments`.

---

## Section A — Branch + MIME parser (pure unit TDD)

### Task A1: Branch

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3g-mime-attachments
```

### Task A2: Failing test

Create `internal/handler/mime_test.go`:

```go
package handler

import (
	"strings"
	"testing"
)

const simpleHTML = `From: alice@example.com
To: bob@example.com
Subject: hi
Date: Fri, 18 Apr 2026 10:30:45 +0000
Content-Type: text/html; charset="utf-8"

<h1>Hello</h1>
`

const altMultipart = `From: "Alice" <alice@example.com>
Subject: Re: project update
Date: Fri, 18 Apr 2026 10:30:45 +0000
Content-Type: multipart/alternative; boundary="ALT"

--ALT
Content-Type: text/plain; charset="utf-8"

Hello from the plain side.
--ALT
Content-Type: text/html; charset="utf-8"

<p>Hello from the <b>HTML</b> side.</p>
--ALT--
`

const mixedWithAttachment = `From: alice@example.com
Subject: with attachment
Date: Fri, 18 Apr 2026 10:30:45 +0000
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/html; charset="utf-8"

<p>See attached.</p>
--MIX
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"
Content-Transfer-Encoding: base64

SGVsbG9QREY=
--MIX--
`

// normalizeCRLF makes fixtures use CRLF line endings like real email.
func normalizeCRLF(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func TestParseMessage_SinglePartHTML(t *testing.T) {
	m, err := parseMessage([]byte(normalizeCRLF(simpleHTML)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(m.HTML) != "<h1>Hello</h1>\r\n" {
		t.Fatalf("html: %q", m.HTML)
	}
	if m.FromEmail != "alice@example.com" {
		t.Fatalf("from_email: %q", m.FromEmail)
	}
	if m.Subject != "hi" {
		t.Fatalf("subject: %q", m.Subject)
	}
}

func TestParseMessage_MultipartAlternative_PrefersHTML(t *testing.T) {
	m, err := parseMessage([]byte(normalizeCRLF(altMultipart)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(string(m.HTML), "<b>HTML</b>") {
		t.Fatalf("html: %q", m.HTML)
	}
	if !strings.Contains(string(m.Text), "plain side") {
		t.Fatalf("text: %q", m.Text)
	}
	if m.FromEmail != "alice@example.com" {
		t.Fatalf("from_email: %q", m.FromEmail)
	}
	if m.Subject != "Re: project update" {
		t.Fatalf("subject: %q", m.Subject)
	}
}

func TestParseMessage_MultipartMixed_ExtractsAttachment(t *testing.T) {
	m, err := parseMessage([]byte(normalizeCRLF(mixedWithAttachment)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(string(m.HTML), "See attached") {
		t.Fatalf("html: %q", m.HTML)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(m.Attachments))
	}
	a := m.Attachments[0]
	if a.Filename != "report.pdf" {
		t.Fatalf("filename: %q", a.Filename)
	}
	if a.ContentType != "application/pdf" {
		t.Fatalf("content_type: %q", a.ContentType)
	}
	if string(a.Content) != "HelloPDF" {
		t.Fatalf("content: %q (want HelloPDF, base64-decoded)", a.Content)
	}
}
```

Run `go test ./internal/handler/...` — expect undefined `parseMessage`, `ParsedMessage`, etc. Capture last 3 lines.

### Task A3: Implement mime.go

Create `internal/handler/mime.go`:

```go
package handler

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

type ParsedMessage struct {
	HTML        []byte
	Text        []byte
	Attachments []Attachment
	Subject     string
	From        string
	FromEmail   string
	ReceivedAt  time.Time
}

type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

func parseMessage(raw []byte) (ParsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, fmt.Errorf("read message: %w", err)
	}

	out := ParsedMessage{
		Subject: msg.Header.Get("Subject"),
		From:    msg.Header.Get("From"),
	}
	if addr, err := mail.ParseAddress(out.From); err == nil {
		out.FromEmail = addr.Address
	}
	if d := msg.Header.Get("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			out.ReceivedAt = t
		}
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}

	headers := textproto.MIMEHeader(msg.Header)
	if err := walkPart(&out, ct, headers, msg.Body); err != nil {
		return ParsedMessage{}, fmt.Errorf("walk: %w", err)
	}
	return out, nil
}

func walkPart(out *ParsedMessage, contentType string, headers textproto.MIMEHeader, body io.Reader) error {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		// Malformed Content-Type: treat as plain text so we don't drop the body.
		mediaType = "text/plain"
		params = map[string]string{}
	}

	// Is this part an attachment? Content-Disposition takes precedence; also
	// accept a `name` parameter on Content-Type as a weaker signal.
	attachName := ""
	isAttachment := false
	if disp := headers.Get("Content-Disposition"); disp != "" {
		d, dp, err := mime.ParseMediaType(disp)
		if err == nil {
			if d == "attachment" {
				isAttachment = true
			}
			if n := dp["filename"]; n != "" {
				attachName = n
				if d != "inline" {
					isAttachment = true
				}
			}
		}
	}
	if attachName == "" && params["name"] != "" && !strings.HasPrefix(mediaType, "multipart/") && !strings.HasPrefix(mediaType, "text/") {
		attachName = params["name"]
		isAttachment = true
	}

	if isAttachment {
		decoded, err := decodePart(headers, body)
		if err != nil {
			return fmt.Errorf("decode attachment: %w", err)
		}
		out.Attachments = append(out.Attachments, Attachment{
			Filename:    attachName,
			ContentType: mediaType,
			Content:     decoded,
		})
		return nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart without boundary")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("next part: %w", err)
			}
			partCT := part.Header.Get("Content-Type")
			if partCT == "" {
				partCT = "text/plain"
			}
			if err := walkPart(out, partCT, part.Header, part); err != nil {
				return err
			}
		}
		return nil
	}

	// Leaf part. Read and decode.
	decoded, err := decodePart(headers, body)
	if err != nil {
		return fmt.Errorf("decode leaf: %w", err)
	}
	switch mediaType {
	case "text/html":
		// Prefer the first HTML part we see (usually the richest in
		// multipart/alternative, though strictly the LAST has priority per RFC.
		// For our purposes first is fine — most emails have exactly one HTML.)
		if len(out.HTML) == 0 {
			out.HTML = decoded
		}
	case "text/plain":
		if len(out.Text) == 0 {
			out.Text = decoded
		}
	}
	return nil
}

func decodePart(headers textproto.MIMEHeader, body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	enc := strings.ToLower(strings.TrimSpace(headers.Get("Content-Transfer-Encoding")))
	switch enc {
	case "base64":
		cleaned := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r", ""), "\n", "")
		return base64.StdEncoding.DecodeString(cleaned)
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	default:
		return raw, nil
	}
}
```

Run tests — expect 3 unit tests pass.

Commit: `feat(handler): add MIME parser with multipart + attachment extraction`

---

## Section B — Path + filename helpers (pure unit TDD)

### Task B1: Failing tests

Append to `internal/handler/paths_test.go`:

```go
func TestSubjectSlug_BasicAndWeirdChars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Re: project update", "re-project-update"},
		{"Your Monthly Statement for Apr 2026!", "your-monthly-statement-for-apr-2026"},
		{"  leading/trailing  ", "leading-trailing"},
		{"", "untitled"},
		{strings.Repeat("a", 80), strings.Repeat("a", 40)},
	}
	for _, c := range cases {
		if got := SubjectSlug(c.in); got != c.want {
			t.Errorf("SubjectSlug(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFromSlug_UsesLocalPart(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alice@example.com", "alice"},
		{"BOB+news@example.com", "bob-news"},
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := FromSlug(c.in); got != c.want {
			t.Errorf("FromSlug(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEmailFolderName_FormatsCorrectly(t *testing.T) {
	tm := time.Date(2026, 4, 18, 10, 30, 45, 0, time.UTC)
	got := EmailFolderName(tm, "Re: project update", "alice@example.com")
	want := "2026-04-18_103045_re-project-update_alice"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSanitizeFilename_StripsSlashesAndDots(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "etc-passwd"},
		{"a/b/c.txt", "a-b-c.txt"},
		{"", "unnamed"},
		{strings.Repeat("x", 300), strings.Repeat("x", 200)},
	}
	for _, c := range cases {
		if got := SanitizeFilename(c.in); got != c.want {
			t.Errorf("SanitizeFilename(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}
```

Add `"strings"` and `"time"` imports to paths_test.go if missing.

Run — expect undefined `SubjectSlug`, `FromSlug`, `EmailFolderName`, `SanitizeFilename`. Capture.

### Task B2: Implement

Append to `internal/handler/paths.go`:

```go
import (
	// existing imports plus:
	"strings"
	"unicode"
)

const (
	maxSubjectSlugLen = 40
	maxFilenameLen    = 200
)

// SubjectSlug lowercases, replaces non-alphanumeric with '-', collapses runs,
// trims dashes, and caps length. Empty input becomes "untitled".
func SubjectSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "untitled"
	}

	var b strings.Builder
	b.Grow(len(s))
	lastDash := true // so we strip leading dashes
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	if len(out) > maxSubjectSlugLen {
		out = strings.TrimRight(out[:maxSubjectSlugLen], "-")
	}
	return out
}

// FromSlug extracts the local part from an email address, lowercases it, and
// replaces '+' and '.' with '-' for readability.
func FromSlug(email string) string {
	if email == "" {
		return "unknown"
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return SubjectSlug(email) // malformed — slug the whole thing
	}
	local := strings.ToLower(email[:at])
	local = strings.ReplaceAll(local, "+", "-")
	local = strings.ReplaceAll(local, ".", "-")
	return local
}

// EmailFolderName returns the per-email folder name used inside the date tree.
// Format: "<YYYY-MM-DD>_<HHMMSS>_<subject-slug>_<from-slug>".
func EmailFolderName(receivedAt time.Time, subject, fromEmail string) string {
	t := receivedAt.UTC()
	return fmt.Sprintf("%04d-%02d-%02d_%02d%02d%02d_%s_%s",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		SubjectSlug(subject),
		FromSlug(fromEmail),
	)
}

// SanitizeFilename strips path separators, null bytes, and leading dots,
// replaces runs of weird chars with '-', and caps length.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed"
	}
	// Strip path components — keep only the base.
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	// Replace invalid chars with '-'.
	var b strings.Builder
	b.Grow(len(name))
	lastDash := false
	for _, r := range name {
		switch {
		case r == 0, r == '/', r == '\\':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			b.WriteRune(r)
			lastDash = false
		}
	}
	out := strings.Trim(b.String(), ". -")
	// If stripping left it empty OR it still looks like a relative path escape,
	// replace aggressively.
	if out == "" || strings.Contains(out, "..") {
		// Collapse any dots and re-clean.
		out = strings.ReplaceAll(out, "..", "-")
		out = strings.Trim(out, ". -")
	}
	if out == "" {
		return "unnamed"
	}
	if len(out) > maxFilenameLen {
		out = out[:maxFilenameLen]
	}
	return out
}
```

(If `fmt` isn't already imported, add it.)

Run — expect 4 new unit tests pass.

Commit: `feat(handler): add SubjectSlug, FromSlug, EmailFolderName, SanitizeFilename`

---

## Section C — RenderHandler + meta.json + attachments (TDD)

### Task C1: Update the existing render test + add multipart test

Modify `internal/handler/fetch_integration_test.go` (or wherever `TestRenderHandler_WritesPDFAndReturnsUpload` lives) to use a multipart/mixed fixture with an attachment. The test should assert:

- `<DataDir>/pdf/<jobID>.pdf` exists with Gotenberg's response bytes
- `<DataDir>/meta/<jobID>.json` exists with the expected subject/from/received_at/attachment_names
- `<DataDir>/attachments/<jobID>/report.pdf` exists with the decoded attachment bytes

Outline:

```go
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
```

Add `"encoding/json"` import if needed.

The existing test `TestRenderHandler_WritesPDFAndReturnsUpload` should ALSO be updated to match the new handler behavior (meta.json is now always written). Adjust its assertions as needed, or replace it with the new multipart test.

Run — expect undefined `handler.RenderMeta` / some fields. Capture.

### Task C2: Implement render.go refactor

Replace `internal/handler/render.go`:

```go
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

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

// RenderMeta is the cross-stage data written by render and read by upload.
type RenderMeta struct {
	Subject         string    `json:"subject"`
	FromEmail       string    `json:"from_email"`
	ReceivedAt      time.Time `json:"received_at"`
	AttachmentNames []string  `json:"attachment_names"`
}

func (h *RenderHandler) Process(ctx context.Context, job store.Job) (string, error) {
	mimePath := filepath.Join(h.cfg.DataDir, "mime", job.ID.String()+".eml") //nolint:gosec // trusted job ID
	rawMime, err := os.ReadFile(mimePath)                                    //nolint:gosec // trusted job ID
	if err != nil {
		return "", fmt.Errorf("read mime: %w", err)
	}

	parsed, err := parseMessage(rawMime)
	if err != nil {
		return "", fmt.Errorf("parse mime: %w", err)
	}

	// Write attachments first (even if render fails, we don't waste them).
	attachDir := filepath.Join(h.cfg.DataDir, "attachments", job.ID.String())
	attachmentNames := make([]string, 0, len(parsed.Attachments))
	if len(parsed.Attachments) > 0 {
		if err := os.MkdirAll(attachDir, 0o750); err != nil {
			return "", fmt.Errorf("mkdir attachments: %w", err)
		}
	}
	for _, a := range parsed.Attachments {
		name := SanitizeFilename(a.Filename)
		if err := os.WriteFile(filepath.Join(attachDir, name), a.Content, 0o600); err != nil {
			return "", fmt.Errorf("write attachment %s: %w", name, err)
		}
		attachmentNames = append(attachmentNames, name)
	}

	// Render the body to PDF.
	html := buildHTMLWrapper(parsed)
	pdfBytes, err := h.client.RenderHTML(ctx, html)
	if err != nil {
		return "", fmt.Errorf("render: %w", err)
	}

	pdfDir := filepath.Join(h.cfg.DataDir, "pdf")
	if err := os.MkdirAll(pdfDir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir pdf: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pdfDir, job.ID.String()+".pdf"), pdfBytes, 0o600); err != nil {
		return "", fmt.Errorf("write pdf: %w", err)
	}

	// Write cross-stage metadata.
	meta := RenderMeta{
		Subject:         parsed.Subject,
		FromEmail:       parsed.FromEmail,
		ReceivedAt:      parsed.ReceivedAt,
		AttachmentNames: attachmentNames,
	}
	if meta.ReceivedAt.IsZero() {
		meta.ReceivedAt = job.CreatedAt
	}
	metaB, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshal meta: %w", err)
	}
	metaDir := filepath.Join(h.cfg.DataDir, "meta")
	if err := os.MkdirAll(metaDir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir meta: %w", err)
	}
	if err := os.WriteFile(filepath.Join(metaDir, job.ID.String()+".json"), metaB, 0o600); err != nil {
		return "", fmt.Errorf("write meta: %w", err)
	}

	return "upload", nil
}

func buildHTMLWrapper(p ParsedMessage) []byte {
	body := p.HTML
	if len(body) == 0 {
		body = []byte("<pre>" + htmlEscape(string(p.Text)) + "</pre>")
	}

	out := &bytes.Buffer{}
	fmt.Fprintf(out, `<!doctype html><html><head><meta charset="utf-8"><title>%s</title></head><body>`, htmlEscape(p.Subject))
	fmt.Fprintf(out, `<div style="font-family:sans-serif;border-bottom:1px solid #ccc;padding-bottom:8px;margin-bottom:12px">`)
	fmt.Fprintf(out, `<div><strong>From:</strong> %s</div>`, htmlEscape(p.From))
	if !p.ReceivedAt.IsZero() {
		fmt.Fprintf(out, `<div><strong>Date:</strong> %s</div>`, htmlEscape(p.ReceivedAt.Format(time.RFC1123)))
	}
	fmt.Fprintf(out, `<div><strong>Subject:</strong> %s</div>`, htmlEscape(p.Subject))
	fmt.Fprintf(out, `</div>`)
	out.Write(body)
	out.WriteString(`</body></html>`)
	return out.Bytes()
}

func htmlEscape(s string) string {
	var r bytes.Buffer
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

Delete `htmlFromMime` (it's replaced by `buildHTMLWrapper` + `parseMessage`).

Run render integration tests — expect all pass.

Commit: `feat(handler): render uses multipart parser, writes attachments + meta.json`

---

## Section D — UploadHandler uses meta.json + per-email folder (TDD)

### Task D1: Update existing upload test

Modify `TestUploadHandler_UploadsToDateTreeAndReturnsTerminal` to also:

- Write a `meta.json` file (same shape as RenderMeta) with subject, from_email, received_at, and attachment_names
- Write attachment files to `<DataDir>/attachments/<jobID>/`
- Assert that the mock Drive server receives `EnsureFolder` calls for year/month/day/email-folder (4 levels), and uploads for email.pdf + each attachment

### Task D2: Implement upload.go refactor

Replace `internal/handler/upload.go`:

```go
package handler

import (
	"context"
	"encoding/json"
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

	// Load cross-stage metadata from render.
	metaPath := filepath.Join(h.cfg.DataDir, "meta", job.ID.String()+".json") //nolint:gosec
	metaB, err := os.ReadFile(metaPath)                                        //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read meta: %w", err)
	}
	var meta RenderMeta
	if err := json.Unmarshal(metaB, &meta); err != nil {
		return "", fmt.Errorf("parse meta: %w", err)
	}
	if meta.ReceivedAt.IsZero() {
		meta.ReceivedAt = job.CreatedAt
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

	// Walk date-tree + email folder.
	folders := append(DateTreeFolders(meta.ReceivedAt), EmailFolderName(meta.ReceivedAt, meta.Subject, meta.FromEmail))
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

	// Upload email.pdf.
	pdfBytes, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "pdf", job.ID.String()+".pdf")) //nolint:gosec
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}
	if _, err := client.Upload(ctx, parent, "email.pdf", "application/pdf", pdfBytes); err != nil {
		return "", fmt.Errorf("upload pdf: %w", err)
	}

	// Upload attachments (if any).
	for _, name := range meta.AttachmentNames {
		data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "attachments", job.ID.String(), name)) //nolint:gosec
		if err != nil {
			return "", fmt.Errorf("read attachment %s: %w", name, err)
		}
		// We don't know the attachment's original content type after writing it
		// to disk without the meta also carrying it. "application/octet-stream"
		// is a safe default; Drive will sniff common types anyway.
		if _, err := client.Upload(ctx, parent, name, "application/octet-stream", data); err != nil {
			return "", fmt.Errorf("upload attachment %s: %w", name, err)
		}
	}

	return "", nil
}
```

Run upload integration test — expect pass.

Commit: `feat(handler): upload reads meta.json, uses per-email folder, uploads attachments`

---

## Section E — Local sweep + PR + tag

```bash
make lint && make test-unit && make test-integration && make k8s-validate && make loadtest
```

All exit 0.

```bash
git push -u origin phase-3g-mime-attachments
gh pr create --base main --head phase-3g-mime-attachments \
  --title "feat(handler): Phase 3G — multipart MIME, attachments, folder-per-email" \
  --body "Phase 3G: make the output look like cloudhq's.

- internal/handler/mime.go: parseMessage walks MIME tree, extracts HTML/text body parts and attachments (with base64/quoted-printable decoding).
- Attachments written to <DataDir>/attachments/<jobID>/<sanitized-name>.
- render.go emits meta.json with subject, from_email, received_at, attachment_names — cross-stage data for upload.
- upload.go reads meta.json, ensures per-email folder named <YYYY-MM-DD>_<HHMMSS>_<subject-slug>_<from-slug>, uploads email.pdf + each attachment.
- Path helpers (SubjectSlug, FromSlug, EmailFolderName, SanitizeFilename) as pure unit-tested functions.
- Final Drive layout:
  <root>/2026/04/18/2026-04-18_103045_re-project-update_alice/email.pdf
  <root>/2026/04/18/2026-04-18_103045_re-project-update_alice/report.pdf

Now genuinely cloudhq-equivalent for emails (minus OCR, inline-image embedding, and exotic multipart/related edge cases)."

PR=\$(gh pr view --json number -q .number)
gh pr merge \$PR --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3g -m "Phase 3G: multipart MIME parsing, attachment preservation, folder-per-email"
git push origin phase-3g
gh release create phase-3g --title "Phase 3G: Real cloudhq-equivalent output" \
  --notes "Proper multipart MIME parsing (pick HTML, fall back to text), attachment preservation, and folder-per-email Drive layout with nice filenames. PDFs now actually say what the email said. Attachments ride along."
git push origin --delete phase-3g-mime-attachments
git branch -D phase-3g-mime-attachments
```
