package gmail

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"

	"github.com/google/uuid"
	"golang.org/x/oauth2"
	gmailapi "google.golang.org/api/gmail/v1"
	"google.golang.org/api/option"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Config struct {
	Store         *store.Store
	UserID        uuid.UUID
	EncryptionKey []byte
	OAuth2        *oauth2.Config
	Endpoint      string
}

type Client struct {
	svc    *gmailapi.Service
	store  *store.Store
	userID uuid.UUID
	key    []byte
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
	svc, err := gmailapi.NewService(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("gmail svc: %w", err)
	}
	return &Client{svc: svc, store: cfg.Store, userID: cfg.UserID, key: cfg.EncryptionKey}, nil
}

func (c *Client) LatestMessageIDs(ctx context.Context, lastHistoryID string) (newIDs []string, newCursor string, err error) {
	startID, err := strconv.ParseUint(lastHistoryID, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("parse history id %q: %w", lastHistoryID, err)
	}

	resp, err := c.svc.Users.History.List("me").
		StartHistoryId(startID).
		HistoryTypes("messageAdded").
		LabelId("INBOX").
		Context(ctx).
		Do()
	if err != nil {
		return nil, "", fmt.Errorf("history list: %w", err)
	}

	for _, h := range resp.History {
		for _, ma := range h.MessagesAdded {
			if ma.Message == nil {
				continue
			}
			if !hasLabel(ma.Message.LabelIds, "CATEGORY_PERSONAL") {
				continue
			}
			newIDs = append(newIDs, ma.Message.Id)
		}
	}

	cursor := lastHistoryID
	if resp.HistoryId != 0 {
		cursor = strconv.FormatUint(resp.HistoryId, 10)
	}
	return newIDs, cursor, nil
}

func (c *Client) FetchMessage(ctx context.Context, messageID string) ([]byte, error) {
	msg, err := c.svc.Users.Messages.Get("me", messageID).Format("raw").Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("get message %s: %w", messageID, err)
	}
	raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(msg.Raw)
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(msg.Raw)
		if err != nil {
			return nil, fmt.Errorf("decode raw: %w", err)
		}
	}
	return raw, nil
}

func hasLabel(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
