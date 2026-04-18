package handler

import (
	"fmt"
	"strings"
	"time"
	"unicode"
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

const (
	maxSubjectSlugLen = 40
	maxFilenameLen    = 200
)

// SubjectSlug lowercases, replaces non-alphanumeric with '-', collapses runs,
// trims dashes, and caps length. Empty input becomes "untitled".
func SubjectSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "untitled"
	}

	var b strings.Builder
	b.Grow(len(s))
	lastDash := true // strip leading dashes
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash {
			b.WriteByte('-')
			lastDash = true
		}
	}
	out := strings.TrimRight(b.String(), "-")
	if out == "" {
		return "untitled"
	}
	if len(out) > maxSubjectSlugLen {
		out = strings.TrimRight(out[:maxSubjectSlugLen], "-")
	}
	return out
}

// FromSlug extracts the local part from an email address, lowercases it, and
// replaces '+' and '.' with '-' for readability.
func FromSlug(email string) string {
	if email == "" {
		return "unknown"
	}
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return SubjectSlug(email)
	}
	local := strings.ToLower(email[:at])
	local = strings.ReplaceAll(local, "+", "-")
	local = strings.ReplaceAll(local, ".", "-")
	return local
}

// EmailFolderName returns the per-email folder name used inside the date tree.
// Format: "<YYYY-MM-DD>_<HHMMSS>_<subject-slug>_<from-slug>".
func EmailFolderName(receivedAt time.Time, subject, fromEmail string) string {
	t := receivedAt.UTC()
	return fmt.Sprintf("%04d-%02d-%02d_%02d%02d%02d_%s_%s",
		t.Year(), int(t.Month()), t.Day(),
		t.Hour(), t.Minute(), t.Second(),
		SubjectSlug(subject),
		FromSlug(fromEmail),
	)
}

// SanitizeFilename replaces path separators and null bytes with '-', collapses
// runs, strips leading/trailing dots and dashes, and caps length.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return "unnamed"
	}
	var b strings.Builder
	b.Grow(len(name))
	lastDash := false
	for _, r := range name {
		switch {
		case r == 0, r == '/', r == '\\':
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		default:
			b.WriteRune(r)
			lastDash = false
		}
	}
	out := strings.Trim(b.String(), ". -")
	if strings.Contains(out, "..") {
		out = strings.ReplaceAll(out, "..", "-")
		out = strings.Trim(out, ". -")
	}
	if out == "" {
		return "unnamed"
	}
	if len(out) > maxFilenameLen {
		out = out[:maxFilenameLen]
	}
	return out
}
