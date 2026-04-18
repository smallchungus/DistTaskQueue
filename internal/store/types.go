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
	ID             uuid.UUID
	UserID         *uuid.UUID
	GmailMessageID *string
	Stage          string
	Status         JobStatus
	Payload        json.RawMessage
	IsSynthetic    bool
	WorkerID       *string
	Attempts       int
	MaxAttempts    int
	LastError      *string
	NextRunAt      time.Time
	ClaimedAt      *time.Time
	CompletedAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type NewJob struct {
	Stage          string
	Payload        json.RawMessage
	UserID         *uuid.UUID
	GmailMessageID *string
	IsSynthetic    bool
}

type User struct {
	ID        uuid.UUID
	Email     string
	CreatedAt time.Time
}

type OAuthToken struct {
	UserID    uuid.UUID
	Provider  string
	AccessCT  []byte
	RefreshCT []byte
	ExpiresAt time.Time
	UpdatedAt time.Time
}
