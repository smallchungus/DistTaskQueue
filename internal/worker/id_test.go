package worker

import (
	"strings"
	"testing"
)

func TestNewWorkerID_HasHostnamePrefixAndRandomSuffix(t *testing.T) {
	id := NewWorkerID()
	if !strings.Contains(id, "-") {
		t.Fatalf("missing hyphen: %q", id)
	}
	parts := strings.SplitN(id, "-", 2)
	if len(parts[0]) == 0 {
		t.Fatalf("empty hostname: %q", id)
	}
	if len(parts[1]) != 16 {
		t.Fatalf("suffix not 16 hex chars: %q", id)
	}
}

func TestNewWorkerID_IsUniqueAcrossCalls(t *testing.T) {
	a := NewWorkerID()
	b := NewWorkerID()
	if a == b {
		t.Fatalf("ids collided: %q", a)
	}
}
