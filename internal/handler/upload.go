package handler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/drive"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type UploadConfig struct {
	Store         *store.Store
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	DriveEndpoint string
	Redis         *goredis.Client
	DataDir       string
	RootFolderID  string
}

type UploadHandler struct {
	cfg   UploadConfig
	cache *drive.FolderCache
}

func NewUploadHandler(cfg UploadConfig) *UploadHandler {
	return &UploadHandler{cfg: cfg, cache: drive.NewFolderCache(cfg.Redis)}
}

func (h *UploadHandler) Process(ctx context.Context, job store.Job) (string, error) {
	if job.UserID == nil {
		return "", errors.New("upload: job missing user_id")
	}

	client, err := drive.New(ctx, drive.Config{
		Store:         h.cfg.Store,
		UserID:        *job.UserID,
		EncryptionKey: h.cfg.EncryptionKey,
		OAuth2:        h.cfg.OAuth2,
		Endpoint:      h.cfg.DriveEndpoint,
	})
	if err != nil {
		return "", fmt.Errorf("drive client: %w", err)
	}

	folders := DateTreeFolders(job.CreatedAt)
	parent := h.cfg.RootFolderID
	pathSoFar := ""
	for _, f := range folders {
		if pathSoFar == "" {
			pathSoFar = f
		} else {
			pathSoFar = path.Join(pathSoFar, f)
		}

		if cached, ok, err := h.cache.Get(ctx, *job.UserID, pathSoFar); err == nil && ok {
			parent = cached
			continue
		}

		folderID, err := client.EnsureFolder(ctx, parent, f)
		if err != nil {
			return "", fmt.Errorf("ensure folder %s: %w", f, err)
		}
		_ = h.cache.Set(ctx, *job.UserID, pathSoFar, folderID, 24*time.Hour)
		parent = folderID
	}

	pdfBytes, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "pdf", job.ID.String()+".pdf"))
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}

	name := job.ID.String() + ".pdf"
	if _, err := client.Upload(ctx, parent, name, "application/pdf", pdfBytes); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	return "", nil
}
