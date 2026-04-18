package pdf

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"time"
)

type Client struct {
	endpoint string
	http     *http.Client
}

func New(endpoint string) *Client {
	return &Client{endpoint: endpoint, http: &http.Client{Timeout: 60 * time.Second}}
}

func (c *Client) RenderHTML(ctx context.Context, html []byte) ([]byte, error) {
	body := &bytes.Buffer{}
	w := multipart.NewWriter(body)

	part, err := w.CreateFormFile("files", "index.html")
	if err != nil {
		return nil, fmt.Errorf("form file: %w", err)
	}
	if _, err := part.Write(html); err != nil {
		return nil, fmt.Errorf("write html: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("close writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/forms/chromium/convert/html", body)
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode/100 != 2 {
		errBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("gotenberg %d: %s", resp.StatusCode, string(errBody))
	}

	pdf, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return pdf, nil
}
