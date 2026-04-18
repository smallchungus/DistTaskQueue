// Package main is a one-off helper to populate the oauth_tokens table with a
// real Google OAuth token for a given email. Run once per user.
//
// Usage:
//
//	./oauth-setup --email=alice@example.com
//
// Environment variables required:
//
//	DATABASE_URL              postgres connection string
//	GOOGLE_OAUTH_CLIENT_ID    from Google Cloud OAuth 2.0 Client IDs page
//	GOOGLE_OAUTH_CLIENT_SECRET
//	TOKEN_ENCRYPTION_KEY      base64 of 32 random bytes (also used by workers)
//
// The redirect URI configured in Google Cloud must include http://localhost:8888/callback.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/smallchungus/disttaskqueue/internal/oauth"
	"github.com/smallchungus/disttaskqueue/internal/store"
)

const callbackPort = "8888"

func main() {
	email := flag.String("email", "", "email address (must match the Google account you'll authorize)")
	flag.Parse()
	if *email == "" {
		fmt.Fprintln(os.Stderr, "--email is required")
		os.Exit(2)
	}

	dsn := mustEnv("DATABASE_URL")
	clientID := mustEnv("GOOGLE_OAUTH_CLIENT_ID")
	clientSecret := mustEnv("GOOGLE_OAUTH_CLIENT_SECRET")
	keyB64 := mustEnv("TOKEN_ENCRYPTION_KEY")

	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil || len(key) != 32 {
		log.Fatalf("TOKEN_ENCRYPTION_KEY must decode to 32 bytes: %v", err)
	}

	ctx := context.Background()
	if err := store.Migrate(ctx, dsn); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("pg connect: %v", err)
	}
	defer pool.Close()
	s := store.New(pool)

	cfg := &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		RedirectURL:  "http://localhost:" + callbackPort + "/callback",
		Scopes:       []string{"https://www.googleapis.com/auth/gmail.readonly", "https://www.googleapis.com/auth/drive.file"},
		Endpoint:     google.Endpoint,
	}

	state := fmt.Sprintf("dtq-%d", time.Now().UnixNano())
	authURL := cfg.AuthCodeURL(state, oauth2.AccessTypeOffline, oauth2.ApprovalForce)

	fmt.Println("Open this URL in your browser:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	fmt.Println("Authorize the app for " + *email + " and complete the redirect to localhost.")
	fmt.Println()

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	mux := http.NewServeMux()
	srv := &http.Server{
		Addr:              ":" + callbackPort,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- errors.New("state mismatch")
			http.Error(w, "state mismatch", http.StatusBadRequest)
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errCh <- errors.New("no code in callback")
			http.Error(w, "no code", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("OK. You can close this tab and return to the terminal."))
		codeCh <- code
	})
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()
	defer func() { _ = srv.Shutdown(context.Background()) }()

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		log.Fatalf("callback: %v", err)
	case <-time.After(5 * time.Minute):
		log.Fatal("timed out waiting for callback")
	}

	tok, err := cfg.Exchange(ctx, code)
	if err != nil {
		log.Fatalf("exchange: %v", err)
	}

	user, err := s.GetUserByEmail(ctx, *email)
	if errors.Is(err, store.ErrUserNotFound) {
		user, err = s.CreateUser(ctx, *email)
		if err != nil {
			log.Fatalf("create user: %v", err)
		}
		fmt.Println("Created user:", user.ID)
	} else if err != nil {
		log.Fatalf("lookup user: %v", err)
	} else {
		fmt.Println("Existing user:", user.ID)
	}

	if err := oauth.SaveToken(ctx, s, user.ID, key, "google", tok); err != nil {
		log.Fatalf("save token: %v", err)
	}
	fmt.Println("Token saved for user", user.ID, "(", *email, "). Scheduler can now sync this account.")
}

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env var: %s", k)
	}
	return v
}
