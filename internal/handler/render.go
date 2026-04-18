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
		if err := os.WriteFile(filepath.Join(attachDir, name), a.Content, 0o600); err != nil { //nolint:gosec // trusted job ID
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
	if err := os.WriteFile(filepath.Join(pdfDir, job.ID.String()+".pdf"), pdfBytes, 0o600); err != nil { //nolint:gosec // trusted job ID
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
	if err := os.WriteFile(filepath.Join(metaDir, job.ID.String()+".json"), metaB, 0o600); err != nil { //nolint:gosec // trusted job ID
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
