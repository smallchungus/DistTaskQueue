package drive

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	driveapi "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

const folderMime = "application/vnd.google-apps.folder"

type Config struct {
	Store         *store.Store
	UserID        uuid.UUID
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	Endpoint      string
}

type Client struct {
	svc *driveapi.Service
}

func New(ctx context.Context, cfg Config) (*Client, error) {
	tok, err := oauth.LoadToken(ctx, cfg.Store, cfg.UserID, cfg.EncryptionKey, "google")
	if err != nil {
		return nil, err
	}

	base := cfg.OAuth2.TokenSource(ctx, tok)
	saving := oauth.NewSavingSource(base, func(t *oauth2.Token) error {
		return oauth.SaveToken(ctx, cfg.Store, cfg.UserID, cfg.EncryptionKey, "google", t)
	}, tok)
	httpClient := oauth2.NewClient(ctx, saving)

	opts := []option.ClientOption{option.WithHTTPClient(httpClient)}
	if cfg.Endpoint != "" {
		opts = append(opts, option.WithEndpoint(cfg.Endpoint))
	}
	svc, err := driveapi.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("drive svc: %w", err)
	}
	return &Client{svc: svc}, nil
}

func (c *Client) EnsureFolder(ctx context.Context, parentID, name string) (string, error) {
	q := fmt.Sprintf("name = '%s' and '%s' in parents and mimeType = '%s' and trashed = false", escape(name), parentID, folderMime)
	resp, err := c.svc.Files.List().Q(q).Fields("files(id,name)").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("list folders: %w", err)
	}
	if len(resp.Files) > 0 {
		return resp.Files[0].Id, nil
	}

	created, err := c.svc.Files.Create(&driveapi.File{
		Name:     name,
		Parents:  []string{parentID},
		MimeType: folderMime,
	}).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("create folder: %w", err)
	}
	return created.Id, nil
}

func (c *Client) Upload(ctx context.Context, parentID, name, contentType string, content []byte) (string, error) {
	created, err := c.svc.Files.Create(&driveapi.File{
		Name:    name,
		Parents: []string{parentID},
	}).Media(bytes.NewReader(content), googleapi.ContentType(contentType)).Fields("id").Context(ctx).Do()
	if err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}
	return created.Id, nil
}

func escape(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, '\\', '\'')
			continue
		}
		out = append(out, s[i])
	}
	return string(out)
}
