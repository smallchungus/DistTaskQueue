package handler

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strings"
	"time"
)

type ParsedMessage struct {
	HTML        []byte
	Text        []byte
	Attachments []Attachment
	Subject     string
	From        string
	FromEmail   string
	ReceivedAt  time.Time
}

type Attachment struct {
	Filename    string
	ContentType string
	Content     []byte
}

func parseMessage(raw []byte) (ParsedMessage, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ParsedMessage{}, fmt.Errorf("read message: %w", err)
	}

	out := ParsedMessage{
		Subject: msg.Header.Get("Subject"),
		From:    msg.Header.Get("From"),
	}
	if addr, err := mail.ParseAddress(out.From); err == nil {
		out.FromEmail = addr.Address
	}
	if d := msg.Header.Get("Date"); d != "" {
		if t, err := mail.ParseDate(d); err == nil {
			out.ReceivedAt = t
		}
	}

	ct := msg.Header.Get("Content-Type")
	if ct == "" {
		ct = "text/plain"
	}

	headers := textproto.MIMEHeader(msg.Header)
	if err := walkPart(&out, ct, headers, msg.Body); err != nil {
		return ParsedMessage{}, fmt.Errorf("walk: %w", err)
	}
	return out, nil
}

func walkPart(out *ParsedMessage, contentType string, headers textproto.MIMEHeader, body io.Reader) error {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = "text/plain"
		params = map[string]string{}
	}

	attachName := ""
	isAttachment := false
	if disp := headers.Get("Content-Disposition"); disp != "" {
		d, dp, err := mime.ParseMediaType(disp)
		if err == nil {
			if d == "attachment" {
				isAttachment = true
			}
			if n := dp["filename"]; n != "" {
				attachName = n
				if d != "inline" {
					isAttachment = true
				}
			}
		}
	}
	if attachName == "" && params["name"] != "" && !strings.HasPrefix(mediaType, "multipart/") && !strings.HasPrefix(mediaType, "text/") {
		attachName = params["name"]
		isAttachment = true
	}

	if isAttachment {
		decoded, err := decodePart(headers, body)
		if err != nil {
			return fmt.Errorf("decode attachment: %w", err)
		}
		out.Attachments = append(out.Attachments, Attachment{
			Filename:    attachName,
			ContentType: mediaType,
			Content:     decoded,
		})
		return nil
	}

	if strings.HasPrefix(mediaType, "multipart/") {
		boundary := params["boundary"]
		if boundary == "" {
			return fmt.Errorf("multipart without boundary")
		}
		mr := multipart.NewReader(body, boundary)
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				return fmt.Errorf("next part: %w", err)
			}
			partCT := part.Header.Get("Content-Type")
			if partCT == "" {
				partCT = "text/plain"
			}
			if err := walkPart(out, partCT, part.Header, part); err != nil {
				return err
			}
		}
		return nil
	}

	decoded, err := decodePart(headers, body)
	if err != nil {
		return fmt.Errorf("decode leaf: %w", err)
	}
	switch mediaType {
	case "text/html":
		if len(out.HTML) == 0 {
			out.HTML = decoded
		}
	case "text/plain":
		if len(out.Text) == 0 {
			out.Text = decoded
		}
	}
	return nil
}

func decodePart(headers textproto.MIMEHeader, body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return nil, err
	}
	enc := strings.ToLower(strings.TrimSpace(headers.Get("Content-Transfer-Encoding")))
	switch enc {
	case "base64":
		cleaned := strings.ReplaceAll(strings.ReplaceAll(string(raw), "\r", ""), "\n", "")
		return base64.StdEncoding.DecodeString(cleaned)
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(bytes.NewReader(raw)))
	default:
		return raw, nil
	}
}
