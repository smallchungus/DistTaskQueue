package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/gmail"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type FetchConfig struct {
	Store         *store.Store
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	GmailEndpoint string
	DataDir       string
}

type FetchHandler struct {
	cfg FetchConfig
}

func NewFetchHandler(cfg FetchConfig) *FetchHandler {
	return &FetchHandler{cfg: cfg}
}

func (h *FetchHandler) Process(ctx context.Context, job store.Job) (string, error) {
	if job.UserID == nil {
		return "", errors.New("fetch: job missing user_id")
	}
	if job.GmailMessageID == nil {
		return "", errors.New("fetch: job missing gmail_message_id")
	}

	client, err := gmail.New(ctx, gmail.Config{
		Store:         h.cfg.Store,
		UserID:        *job.UserID,
		EncryptionKey: h.cfg.EncryptionKey,
		OAuth2:        h.cfg.OAuth2,
		Endpoint:      h.cfg.GmailEndpoint,
	})
	if err != nil {
		return "", fmt.Errorf("gmail client: %w", err)
	}

	raw, err := client.FetchMessage(ctx, *job.GmailMessageID)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}

	dir := filepath.Join(h.cfg.DataDir, "mime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	path := filepath.Join(dir, job.ID.String()+".eml")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", fmt.Errorf("write mime: %w", err)
	}

	return "render", nil
}
