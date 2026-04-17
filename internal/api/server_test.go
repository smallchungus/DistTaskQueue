package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouter_WiresHealthzAndVersion(t *testing.T) {
	r := NewRouter(Config{Version: "v0.1.0", Commit: "abc123"})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	cases := []struct {
		path     string
		wantBody string
	}{
		{"/healthz", "ok"},
		{"/version", `{"version":"v0.1.0","commit":"abc123"}`},
	}

	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			resp, err := http.Get(srv.URL + c.path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = resp.Body.Close() })

			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status: got %d", resp.StatusCode)
			}
			body, _ := io.ReadAll(resp.Body)
			if string(body) != c.wantBody {
				t.Fatalf("body: got %q, want %q", string(body), c.wantBody)
			}
		})
	}
}
