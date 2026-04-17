package api

import (
	"encoding/json"
	"net/http"
)

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

type versionPayload struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func versionHandler(version, commit string) http.HandlerFunc {
	body, _ := json.Marshal(versionPayload{Version: version, Commit: commit})
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}
}
