package handler

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// DateTreeFolders returns human-readable year / month / day folder names for
// the given time. Format:
//
//	["2026", "April 2026", "18 April 2026 (Saturday)"]
//
// Year stays a 4-digit number so it sorts correctly under the root. Month and
// day folders embed the full name + year for readability when browsing Drive.
func DateTreeFolders(t time.Time) []string {
	t = t.UTC()
	month := t.Month().String()
	weekday := t.Weekday().String()
	return []string{
		fmt.Sprintf("%04d", t.Year()),
		fmt.Sprintf("%s %04d", month, t.Year()),
		fmt.Sprintf("%d %s %04d (%s)", t.Day(), month, t.Year(), weekday),
	}
}

const (
	maxSubjectFolderLen = 80
	maxFilenameLen      = 200
)

// EmailFolderName returns the per-email folder name used inside the day folder.
// Format: "<subject> - <sender> (HH:MM)". The time suffix disambiguates
// folders when two emails share a subject + sender in the same day.
// Subject is sanitized (illegal Drive chars stripped, whitespace collapsed)
// and truncated. Original casing and spaces preserved.
func EmailFolderName(receivedAt time.Time, subject, fromEmail string) string {
	t := receivedAt.UTC()
	subj := cleanSubject(subject)
	sender := strings.TrimSpace(fromEmail)
	if sender == "" {
		sender = "unknown sender"
	}
	return fmt.Sprintf("%s - %s (%02d:%02d)", subj, sender, t.Hour(), t.Minute())
}

// cleanSubject strips chars illegal in Drive folder names, collapses whitespace,
// strips leading/trailing punctuation, and caps length. Preserves casing.
// Empty or all-illegal input becomes "(no subject)".
func cleanSubject(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "(no subject)"
	}

	var b strings.Builder
	b.Grow(len(s))
	lastSpace := false
	for _, r := range s {
		switch r {
		case 0, '/', '\\':
			if !lastSpace {
				b.WriteByte(' ')
				lastSpace = true
			}
		default:
			if unicode.IsSpace(r) {
				if !lastSpace {
					b.WriteByte(' ')
					lastSpace = true
				}
				continue
			}
			b.WriteRune(r)
			lastSpace = false
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Trim(out, ".-_")
	if out == "" {
		return "(no subject)"
	}
	if len(out) > maxSubjectFolderLen {
		out = strings.TrimSpace(out[:maxSubjectFolderLen])
	}
	return out
}

// SubjectSlug is retained for legacy callers / tests. New code should prefer
// the human-readable cleanSubject path used by EmailFolderName.
func SubjectSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "untitled"
	}

	var b strings.Builder
	b.Grow(len(s))
	lastDash := true
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
	if len(out) > 40 {
		out = strings.TrimRight(out[:40], "-")
	}
	return out
}

// FromSlug is retained for legacy callers / tests.
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
		switch r {
		case 0, '/', '\\':
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
