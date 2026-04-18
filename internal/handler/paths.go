package handler

import (
	"fmt"
	"time"
)

// DateTreeFolders returns the year/month/day folder names for the given time.
func DateTreeFolders(t time.Time) []string {
	t = t.UTC()
	return []string{
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%02d", int(t.Month())),
		fmt.Sprintf("%02d", t.Day()),
	}
}
