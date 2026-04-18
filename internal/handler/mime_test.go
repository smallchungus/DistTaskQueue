package handler

import (
	"strings"
	"testing"
)

const simpleHTML = `From: alice@example.com
To: bob@example.com
Subject: hi
Date: Fri, 18 Apr 2026 10:30:45 +0000
Content-Type: text/html; charset="utf-8"

<h1>Hello</h1>
`

const altMultipart = `From: "Alice" <alice@example.com>
Subject: Re: project update
Date: Fri, 18 Apr 2026 10:30:45 +0000
Content-Type: multipart/alternative; boundary="ALT"

--ALT
Content-Type: text/plain; charset="utf-8"

Hello from the plain side.
--ALT
Content-Type: text/html; charset="utf-8"

<p>Hello from the <b>HTML</b> side.</p>
--ALT--
`

const mixedWithAttachment = `From: alice@example.com
Subject: with attachment
Date: Fri, 18 Apr 2026 10:30:45 +0000
Content-Type: multipart/mixed; boundary="MIX"

--MIX
Content-Type: text/html; charset="utf-8"

<p>See attached.</p>
--MIX
Content-Type: application/pdf; name="report.pdf"
Content-Disposition: attachment; filename="report.pdf"
Content-Transfer-Encoding: base64

SGVsbG9QREY=
--MIX--
`

// normalizeCRLF makes fixtures use CRLF line endings like real email.
func normalizeCRLF(s string) string {
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func TestParseMessage_SinglePartHTML(t *testing.T) {
	m, err := parseMessage([]byte(normalizeCRLF(simpleHTML)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if string(m.HTML) != "<h1>Hello</h1>\r\n" {
		t.Fatalf("html: %q", m.HTML)
	}
	if m.FromEmail != "alice@example.com" {
		t.Fatalf("from_email: %q", m.FromEmail)
	}
	if m.Subject != "hi" {
		t.Fatalf("subject: %q", m.Subject)
	}
}

func TestParseMessage_MultipartAlternative_PrefersHTML(t *testing.T) {
	m, err := parseMessage([]byte(normalizeCRLF(altMultipart)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(string(m.HTML), "<b>HTML</b>") {
		t.Fatalf("html: %q", m.HTML)
	}
	if !strings.Contains(string(m.Text), "plain side") {
		t.Fatalf("text: %q", m.Text)
	}
	if m.FromEmail != "alice@example.com" {
		t.Fatalf("from_email: %q", m.FromEmail)
	}
	if m.Subject != "Re: project update" {
		t.Fatalf("subject: %q", m.Subject)
	}
}

func TestParseMessage_MultipartMixed_ExtractsAttachment(t *testing.T) {
	m, err := parseMessage([]byte(normalizeCRLF(mixedWithAttachment)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(string(m.HTML), "See attached") {
		t.Fatalf("html: %q", m.HTML)
	}
	if len(m.Attachments) != 1 {
		t.Fatalf("attachments: got %d, want 1", len(m.Attachments))
	}
	a := m.Attachments[0]
	if a.Filename != "report.pdf" {
		t.Fatalf("filename: %q", a.Filename)
	}
	if a.ContentType != "application/pdf" {
		t.Fatalf("content_type: %q", a.ContentType)
	}
	if string(a.Content) != "HelloPDF" {
		t.Fatalf("content: %q (want HelloPDF, base64-decoded)", a.Content)
	}
}
