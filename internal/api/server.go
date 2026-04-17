package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"
)

type Config struct {
	Version string
	Commit  string
}

func NewRouter(cfg Config) http.Handler {
	r := chi.NewRouter()
	r.Get("/healthz", healthz)
	r.Get("/version", versionHandler(cfg.Version, cfg.Commit))
	return r
}
