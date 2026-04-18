package store

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type JobStatus string

const (
	StatusQueued  JobStatus = "queued"
	StatusRunning JobStatus = "running"
	StatusDone    JobStatus = "done"
	StatusDead    JobStatus = "dead"
)

type Job struct {
	ID          uuid.UUID
	Stage       string
	Status      JobStatus
	Payload     json.RawMessage
	WorkerID    *string
	Attempts    int
	MaxAttempts int
	LastError   *string
	NextRunAt   time.Time
	ClaimedAt   *time.Time
	CompletedAt *time.Time
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

type NewJob struct {
	Stage   string
	Payload json.RawMessage
}
