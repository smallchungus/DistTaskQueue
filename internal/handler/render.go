package handler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/mail"
	"os"
	"path/filepath"
	"strings"

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
	rawMime, err := os.ReadFile(mimePath) //nolint:gosec // path derived from trusted job ID, not user input
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
	if err := os.MkdirAll(pdfDir, 0o750); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	pdfPath := filepath.Join(pdfDir, job.ID.String()+".pdf")
	if err := os.WriteFile(pdfPath, pdfBytes, 0o600); err != nil {
		return "", fmt.Errorf("write pdf: %w", err)
	}

	return "upload", nil
}

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
	if contentType == "" || strings.HasPrefix(contentType, "text/html") {
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
