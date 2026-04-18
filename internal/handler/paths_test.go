package handler

import (
	"reflect"
	"testing"
	"time"
)

func TestDateTreeFolders(t *testing.T) {
	got := DateTreeFolders(time.Date(2026, 4, 17, 10, 30, 0, 0, time.UTC))
	want := []string{"2026", "04", "17"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
