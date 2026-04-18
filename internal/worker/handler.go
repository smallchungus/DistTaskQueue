package worker

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Handler interface {
	Process(ctx context.Context, job store.Job) (nextStage string, err error)
}

type NoopHandler struct{}

func (NoopHandler) Process(ctx context.Context, job store.Job) (string, error) {
	var p struct {
		SleepMs  int     `json:"sleepMs"`
		FailRate float64 `json:"failRate"`
	}
	if len(job.Payload) > 0 {
		_ = json.Unmarshal(job.Payload, &p)
	}

	// Honor the sleep first. If context cancels mid-sleep, propagate that.
	if p.SleepMs > 0 {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(p.SleepMs) * time.Millisecond):
		}
	}

	// Then optionally fail (synthetic demo: "flaky" button fires this).
	// Uses math/rand/v2 — not security-sensitive, just statistical behavior.
	if p.FailRate > 0 && rand.Float64() < p.FailRate { //nolint:gosec
		return "", errNoopFlaky
	}
	return "", nil
}

var errNoopFlaky = noopFlakyError{}

type noopFlakyError struct{}

func (noopFlakyError) Error() string { return "synthetic flaky failure" }
