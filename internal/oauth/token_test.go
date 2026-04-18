package oauth

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

type sequenceSource struct {
	tokens []*oauth2.Token
	idx    int
	err    error
}

func (s *sequenceSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	if s.idx >= len(s.tokens) {
		return s.tokens[len(s.tokens)-1], nil
	}
	tok := s.tokens[s.idx]
	s.idx++
	return tok, nil
}

func TestSavingSource_SavesWhenTokenChanges(t *testing.T) {
	tok1 := &oauth2.Token{AccessToken: "v1", RefreshToken: "r1", Expiry: time.Now().Add(time.Hour)}
	tok2 := &oauth2.Token{AccessToken: "v2", RefreshToken: "r1", Expiry: time.Now().Add(2 * time.Hour)}

	base := &sequenceSource{tokens: []*oauth2.Token{tok1, tok2}}
	var saved []*oauth2.Token
	src := NewSavingSource(base, func(t *oauth2.Token) error {
		saved = append(saved, t)
		return nil
	}, tok1)

	got, err := src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "v1" {
		t.Fatalf("first token: %s", got.AccessToken)
	}
	if len(saved) != 0 {
		t.Fatalf("save called for unchanged token: %d", len(saved))
	}

	got, err = src.Token()
	if err != nil {
		t.Fatal(err)
	}
	if got.AccessToken != "v2" {
		t.Fatalf("second token: %s", got.AccessToken)
	}
	if len(saved) != 1 || saved[0].AccessToken != "v2" {
		t.Fatalf("save not called with refreshed token: %+v", saved)
	}
}

func TestSavingSource_PropagatesBaseError(t *testing.T) {
	base := &sequenceSource{err: errors.New("refresh failed")}
	src := NewSavingSource(base, func(*oauth2.Token) error { return nil }, nil)
	if _, err := src.Token(); err == nil {
		t.Fatal("expected error")
	}
}

func TestEncryptToken_DecryptToken_RoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	in := &oauth2.Token{
		AccessToken:  "the-access",
		RefreshToken: "the-refresh",
		Expiry:       time.Now().Add(time.Hour).UTC().Truncate(time.Second),
	}

	accessCT, refreshCT, err := EncryptToken(in, key)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	out, err := DecryptToken(accessCT, refreshCT, in.Expiry, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if out.AccessToken != in.AccessToken || out.RefreshToken != in.RefreshToken {
		t.Fatalf("mismatch: %+v vs %+v", out, in)
	}
}
