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
