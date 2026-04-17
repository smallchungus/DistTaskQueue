package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_ReturnsOK(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	healthz(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	if body := rec.Body.String(); body != "ok" {
		t.Fatalf("body: got %q, want %q", body, "ok")
	}
}

func TestVersion_ReturnsConfiguredVersion(t *testing.T) {
	h := versionHandler("v0.1.0", "abc123")

	req := httptest.NewRequest(http.MethodGet, "/version", nil)
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want %d", rec.Code, http.StatusOK)
	}
	want := `{"version":"v0.1.0","commit":"abc123"}`
	if got := rec.Body.String(); got != want {
		t.Fatalf("body: got %q, want %q", got, want)
	}
}
