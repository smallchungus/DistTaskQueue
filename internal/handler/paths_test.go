package handler

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDateTreeFolders(t *testing.T) {
	got := DateTreeFolders(time.Date(2026, 4, 18, 10, 30, 0, 0, time.UTC))
	want := []string{"2026", "April 2026", "18 April 2026 (Saturday)"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSubjectSlug_BasicAndWeirdChars(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"Re: project update", "re-project-update"},
		{"Your Monthly Statement for Apr 2026!", "your-monthly-statement-for-apr-2026"},
		{"  leading/trailing  ", "leading-trailing"},
		{"", "untitled"},
		{strings.Repeat("a", 80), strings.Repeat("a", 40)},
	}
	for _, c := range cases {
		if got := SubjectSlug(c.in); got != c.want {
			t.Errorf("SubjectSlug(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFromSlug_UsesLocalPart(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"alice@example.com", "alice"},
		{"BOB+news@example.com", "bob-news"},
		{"", "unknown"},
	}
	for _, c := range cases {
		if got := FromSlug(c.in); got != c.want {
			t.Errorf("FromSlug(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEmailFolderName_FormatsCorrectly(t *testing.T) {
	tm := time.Date(2026, 4, 18, 10, 30, 45, 0, time.UTC)
	got := EmailFolderName(tm, "Re: project update", "alice@example.com")
	want := "Re: project update - alice@example.com (10:30)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEmailFolderName_SanitizesWeirdChars(t *testing.T) {
	tm := time.Date(2026, 4, 18, 10, 30, 0, 0, time.UTC)
	got := EmailFolderName(tm, "Subject with / slash and  extra    spaces", "bob@example.com")
	want := "Subject with slash and extra spaces - bob@example.com (10:30)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestEmailFolderName_EmptySubjectFallback(t *testing.T) {
	tm := time.Date(2026, 4, 18, 10, 30, 0, 0, time.UTC)
	got := EmailFolderName(tm, "   ", "alice@example.com")
	want := "(no subject) - alice@example.com (10:30)"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestSanitizeFilename_StripsSlashesAndDots(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"report.pdf", "report.pdf"},
		{"../../etc/passwd", "etc-passwd"},
		{"a/b/c.txt", "a-b-c.txt"},
		{"", "unnamed"},
		{strings.Repeat("x", 300), strings.Repeat("x", 200)},
	}
	for _, c := range cases {
		if got := SanitizeFilename(c.in); got != c.want {
			t.Errorf("SanitizeFilename(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}
