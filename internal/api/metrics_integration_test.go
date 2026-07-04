//go:build integration

package api_test

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/api"
	"github.com/smallchungus/disttaskqueue/internal/queue"
	"github.com/smallchungus/disttaskqueue/internal/store"
	"github.com/smallchungus/disttaskqueue/internal/testutil"
)

var lastPollLine = regexp.MustCompile(`(?m)^dtq_last_poll_success_timestamp_seconds ([0-9.e+]+)$`)

func TestMetrics_ExportsPollFreshness(t *testing.T) {
	ctx := context.Background()
	pool := testutil.StartPostgres(t)
	if err := store.Migrate(ctx, pool.Config().ConnString()); err != nil {
		t.Fatal(err)
	}
	s := store.New(pool)
	redisCli := testutil.StartRedis(t)
	q := queue.New(redisCli)

	u, err := s.CreateUser(ctx, fmt.Sprintf("metrics+%d@example.com", time.Now().UnixNano()))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetGmailSyncState(ctx, u.ID, "1"); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(api.NewRouter(api.Config{Store: s, Queue: q, Redis: redisCli}))
	defer srv.Close()

	deadline := time.Now().Add(15 * time.Second)
	var lastBody string
	for time.Now().Before(deadline) {
		resp, err := http.Get(srv.URL + "/metrics")
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		lastBody = string(body)

		if m := lastPollLine.FindStringSubmatch(lastBody); m != nil {
			v, err := strconv.ParseFloat(m[1], 64)
			if err != nil {
				t.Fatalf("parse gauge %q: %v", m[1], err)
			}
			age := time.Since(time.Unix(int64(v), 0))
			if age >= 0 && age < time.Minute {
				return
			}
			// zero until the collector's first refresh lands; keep polling
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("dtq_last_poll_success_timestamp_seconds never exported; body:\n%.600s", lastBody)
}
