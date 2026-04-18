package worker

import (
	"context"
	"encoding/json"
	"time"

	"github.com/smallchungus/disttaskqueue/internal/store"
)

type Handler interface {
	Process(ctx context.Context, job store.Job) (nextStage string, err error)
}

type NoopHandler struct{}

func (NoopHandler) Process(ctx context.Context, job store.Job) (string, error) {
	var p struct {
		SleepMs int `json:"sleepMs"`
	}
	if len(job.Payload) > 0 {
		_ = json.Unmarshal(job.Payload, &p)
	}
	if p.SleepMs <= 0 {
		return "", nil
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Duration(p.SleepMs) * time.Millisecond):
		return "", nil
	}
}
