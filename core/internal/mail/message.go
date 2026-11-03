package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"time"
)

// Message is the content of an email: a subject and the same text as plain
// text and HTML (multipart/alternative).
type Message struct {
	Subject string `json:"subject"`
	Text    string `json:"text"`
	HTML    string `json:"html"`
}

// ErrInvalidAddress is returned for addresses that cannot be used in a
// message.
var ErrInvalidAddress = errors.New("mail: invalid address")

// ParseAddress parses a sender such as "Acme <no-reply@acme.com>" or a bare
// address. Header injection is impossible: line breaks are rejected.
func ParseAddress(s string) (*mail.Address, error) {
	if strings.ContainsAny(s, "\r\n") {
		return nil, ErrInvalidAddress
	}
	a, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil || strings.Count(a.Address, "@") != 1 || strings.ContainsAny(a.Address, " \t") {
		return nil, ErrInvalidAddress
	}
	return a, nil
}

// ParseRecipient parses a bare recipient address such as maya@example.com.
func ParseRecipient(s string) (string, error) {
	a, err := ParseAddress(s)
	if err != nil || a.Name != "" || a.Address != strings.TrimSpace(s) || len(a.Address) > 254 {
		return "", ErrInvalidAddress
	}
	return a.Address, nil
}

// encode renders the message in RFC 5322 form with CRLF line endings.
func encode(from *mail.Address, to string, m Message, date time.Time) ([]byte, error) {
	if strings.ContainsAny(m.Subject, "\r\n") {
		return nil, errors.New("mail: the subject contains a line break")
	}
	boundary, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	id, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	_, domain, _ := strings.Cut(from.Address, "@")

	var b bytes.Buffer
	header := func(k, v string) { fmt.Fprintf(&b, "%s: %s\r\n", k, v) }
	header("From", from.String())
	header("To", (&mail.Address{Address: to}).String())
	header("Subject", mime.QEncoding.Encode("utf-8", m.Subject))
	header("Date", date.UTC().Format(time.RFC1123Z))
	header("Message-ID", "<"+id+"@"+domain+">")
	header("MIME-Version", "1.0")
	// System email: no auto-replies (RFC 3834).
	header("Auto-Submitted", "auto-generated")
	header("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")
	for _, part := range []struct{ contentType, body string }{
		{"text/plain; charset=utf-8", m.Text},
		{"text/html; charset=utf-8", m.HTML},
	} {
		b.WriteString("--" + boundary + "\r\n")
		header("Content-Type", part.contentType)
		header("Content-Transfer-Encoding", "quoted-printable")
		b.WriteString("\r\n")
		qp := quotedprintable.NewWriter(&b)
		if _, err := qp.Write([]byte(crlf(part.body))); err != nil {
			return nil, err
		}
		if err := qp.Close(); err != nil {
			return nil, err
		}
		b.WriteString("\r\n")
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

// crlf normalizes line endings to CRLF.
func crlf(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.ReplaceAll(s, "\n", "\r\n")
}

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
