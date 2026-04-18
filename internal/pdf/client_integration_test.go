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
