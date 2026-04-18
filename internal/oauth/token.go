package oauth

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"golang.org/x/oauth2"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

func LoadToken(ctx context.Context, s *store.Store, userID uuid.UUID, key []byte, provider string) (*oauth2.Token, error) {
	rec, err := s.GetOAuthToken(ctx, userID, provider)
	if err != nil {
		return nil, fmt.Errorf("load token: %w", err)
	}
	return DecryptToken(rec.AccessCT, rec.RefreshCT, rec.ExpiresAt, key)
}

func SaveToken(ctx context.Context, s *store.Store, userID uuid.UUID, key []byte, provider string, tok *oauth2.Token) error {
	accessCT, refreshCT, err := EncryptToken(tok, key)
	if err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	return s.SaveOAuthToken(ctx, store.OAuthToken{
		UserID:    userID,
		Provider:  provider,
		AccessCT:  accessCT,
		RefreshCT: refreshCT,
		ExpiresAt: tok.Expiry,
	})
}

func EncryptToken(tok *oauth2.Token, key []byte) (access, refresh []byte, err error) {
	access, err = Encrypt([]byte(tok.AccessToken), key)
	if err != nil {
		return nil, nil, err
	}
	refresh, err = Encrypt([]byte(tok.RefreshToken), key)
	if err != nil {
		return nil, nil, err
	}
	return access, refresh, nil
}

func DecryptToken(access, refresh []byte, expiry time.Time, key []byte) (*oauth2.Token, error) {
	a, err := Decrypt(access, key)
	if err != nil {
		return nil, fmt.Errorf("decrypt access: %w", err)
	}
	r, err := Decrypt(refresh, key)
	if err != nil {
		return nil, fmt.Errorf("decrypt refresh: %w", err)
	}
	return &oauth2.Token{
		AccessToken:  string(a),
		RefreshToken: string(r),
		TokenType:    "Bearer",
		Expiry:       expiry,
	}, nil
}

type savingSource struct {
	base oauth2.TokenSource
	save func(*oauth2.Token) error
	last *oauth2.Token
}

func NewSavingSource(base oauth2.TokenSource, save func(*oauth2.Token) error, seed *oauth2.Token) oauth2.TokenSource {
	return &savingSource{base: base, save: save, last: seed}
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	tok, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	if s.last == nil || tok.AccessToken != s.last.AccessToken {
		if err := s.save(tok); err != nil {
			return nil, fmt.Errorf("save: %w", err)
		}
		s.last = tok
	}
	return tok, nil
}
