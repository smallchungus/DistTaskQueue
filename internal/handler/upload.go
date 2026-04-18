package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
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
	// RootFolderID is the starting Drive folder ID. If empty, defaults to "root"
	// (the user's My Drive root).
	RootFolderID string
	// RootPath is an optional slash-delimited folder path under RootFolderID
	// that gets created/resolved once and used as the parent of the date tree.
	// Example: "02_GmailBackup/DistTaskQueue Backup".
	RootPath string
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

	// Load cross-stage metadata from render.
	metaPath := filepath.Join(h.cfg.DataDir, "meta", job.ID.String()+".json") //nolint:gosec // trusted job ID
	metaB, err := os.ReadFile(metaPath)                                       //nolint:gosec // trusted job ID
	if err != nil {
		return "", fmt.Errorf("read meta: %w", err)
	}
	var meta RenderMeta
	if err := json.Unmarshal(metaB, &meta); err != nil {
		return "", fmt.Errorf("parse meta: %w", err)
	}
	if meta.ReceivedAt.IsZero() {
		meta.ReceivedAt = job.CreatedAt
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

	// Walk the full folder chain: [prefix (from RootPath)...] + YYYY/MM/DD/<email-folder>.
	// Starting parent is RootFolderID, or "root" (Drive alias for My Drive root).
	parent := h.cfg.RootFolderID
	if parent == "" {
		parent = "root"
	}

	var folders []string
	if h.cfg.RootPath != "" {
		for _, p := range strings.Split(h.cfg.RootPath, "/") {
			if p = strings.TrimSpace(p); p != "" {
				folders = append(folders, p)
			}
		}
	}
	folders = append(folders, DateTreeFolders(meta.ReceivedAt)...)
	folders = append(folders, EmailFolderName(meta.ReceivedAt, meta.Subject, meta.FromEmail))

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

	// Upload email.pdf.
	pdfBytes, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "pdf", job.ID.String()+".pdf")) //nolint:gosec // trusted job ID
	if err != nil {
		return "", fmt.Errorf("read pdf: %w", err)
	}
	if _, err := client.Upload(ctx, parent, "email.pdf", "application/pdf", pdfBytes); err != nil {
		return "", fmt.Errorf("upload pdf: %w", err)
	}

	// Upload attachments (if any).
	for _, name := range meta.AttachmentNames {
		data, err := os.ReadFile(filepath.Join(h.cfg.DataDir, "attachments", job.ID.String(), name)) //nolint:gosec // trusted job ID
		if err != nil {
			return "", fmt.Errorf("read attachment %s: %w", name, err)
		}
		if _, err := client.Upload(ctx, parent, name, "application/octet-stream", data); err != nil {
			return "", fmt.Errorf("upload attachment %s: %w", name, err)
		}
	}

	return "", nil
}
