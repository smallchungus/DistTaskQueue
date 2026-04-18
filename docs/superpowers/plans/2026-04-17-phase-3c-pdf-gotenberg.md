# Phase 3C — Gotenberg PDF Rendering

> Use `superpowers:subagent-driven-development`. `- [ ]` checkboxes track steps.

**Goal:** A small `internal/pdf` package that POSTs HTML to a Gotenberg sidecar and returns the PDF bytes. Add Gotenberg to `docker-compose.yaml`. Tests use `httptest.Server` (no real Gotenberg dependency in CI).

**Architecture:**

```
internal/pdf/
  client.go               Client struct, New(endpoint), RenderHTML(ctx, html) ([]byte, error)
  client_integration_test.go    httptest mocks Gotenberg's /forms/chromium/convert/html
docker-compose.yaml       (modified) add gotenberg service
```

Gotenberg API: POST `/forms/chromium/convert/html` with multipart form containing a file part named `index.html`. Response body is `application/pdf`.

**Branch:** `phase-3c-pdf-gotenberg`.

---

## Section A — Branch + Client + RenderHTML (TDD)

### Task A1: Branch

```bash
cd /Users/willchen/Development/DistTaskQueue
git checkout main && git pull --ff-only
git checkout -b phase-3c-pdf-gotenberg
```

### Task A2: Failing test

Create `internal/pdf/client_integration_test.go`:

```go
//go:build integration

package pdf_test

import (
	"context"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/smallchungus/disttaskqueue/internal/pdf"
)

func TestRenderHTML_PostsMultipartAndReturnsPDF(t *testing.T) {
	const fakePDF = "%PDF-1.7\nfake\n%%EOF"
	var gotForm map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/forms/chromium/convert/html" {
			t.Errorf("path: %s", r.URL.Path)
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil {
			t.Fatalf("ct: %v", err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		gotForm = map[string]string{}
		for {
			p, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(p)
			gotForm[p.FileName()] = string(b)
		}
		w.Header().Set("Content-Type", "application/pdf")
		_, _ = w.Write([]byte(fakePDF))
	}))
	defer srv.Close()

	c := pdf.New(srv.URL)
	got, err := c.RenderHTML(context.Background(), []byte("<html><body>hi</body></html>"))
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if string(got) != fakePDF {
		t.Fatalf("got %q, want %q", got, fakePDF)
	}
	if v := gotForm["index.html"]; v != "<html><body>hi</body></html>" {
		t.Fatalf("index.html: %q", v)
	}
}

func TestRenderHTML_ReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("bad html"))
	}))
	defer srv.Close()

	c := pdf.New(srv.URL)
	_, err := c.RenderHTML(context.Background(), []byte("garbage"))
	if err == nil {
		t.Fatal("expected error")
	}
}
```

Run: `go test -tags=integration ./internal/pdf/...` — expect undefined `pdf.New`, `pdf.Client`, `c.RenderHTML`. Capture last 3 lines.

### Task A3: Implement Client

Create `internal/pdf/client.go`:

```go
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
```

Run integration tests — expect 2 pass.

Commit: `feat(pdf): add Gotenberg client with RenderHTML`

---

## Section B — docker-compose Gotenberg service

Append to `docker-compose.yaml` `services:` block (after `redis`):

```yaml
  gotenberg:
    image: gotenberg/gotenberg:8
    ports:
      - "3000:3000"
```

(Gotenberg's `/health` endpoint requires extra config; just expose the port — apps that depend on it should retry with backoff.)

Verify locally:

```bash
docker compose up -d gotenberg
sleep 8
curl -s http://localhost:3000/health
docker compose down -v
```

Expected: a JSON status (HTTP 200 with `{"status":"up",...}` shape).

Commit: `chore(compose): add gotenberg PDF render sidecar`

---

## Section C — Local sweep + PR

```bash
make lint && make test-unit && make test-integration && make k8s-validate && make loadtest
```

All exit 0.

```bash
git push -u origin phase-3c-pdf-gotenberg
gh pr create --base main --head phase-3c-pdf-gotenberg \
  --title "feat(pdf): Phase 3C — Gotenberg HTML-to-PDF client" \
  --body "Phase 3C: tiny Gotenberg client. POST multipart form with index.html, get PDF bytes back. Plus the gotenberg sidecar in docker-compose.yaml. Tests use httptest; no real Gotenberg in CI."

PR=$(gh pr view --json number -q .number)
gh pr merge $PR --auto --squash
```

After merge:

```bash
git checkout main && git pull --ff-only
git tag -a phase-3c -m "Phase 3C: Gotenberg PDF client"
git push origin phase-3c
gh release create phase-3c --title "Phase 3C: Gotenberg PDF Rendering" \
  --notes "Tiny HTTP client wrapping Gotenberg's Chromium HTML-to-PDF endpoint. Plus docker-compose sidecar."
git push origin --delete phase-3c-pdf-gotenberg
git branch -D phase-3c-pdf-gotenberg
```
