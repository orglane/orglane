package mail

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"net/smtp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestInvitationTemplateEscapesContent(t *testing.T) {
	m, err := RenderInvitation("fr-FR, en;q=0.9", Invitation{
		Name: `<script>alert("x")</script>`, Inviter: "Alex", Workspace: "work.acme.test",
		WorkEmail: "maya@acme.test", Link: "https://work.acme.test/invite#oli_example", Days: 7,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(m.HTML, "<script>") || !strings.Contains(m.HTML, "&lt;script&gt;") || !strings.Contains(m.HTML, `lang="en"`) {
		t.Fatalf("unsafe or incorrectly localized HTML: %s", m.HTML)
	}
	for _, body := range []string{m.Text, m.HTML} {
		if !strings.Contains(body, "https://work.acme.test/invite#oli_example") || !strings.Contains(body, "maya@acme.test") {
			t.Fatalf("incomplete invitation: %s", body)
		}
	}
	if strings.Contains(m.HTML, "<img") || strings.Contains(m.HTML, "<style") {
		t.Fatal("email must not load remote content")
	}
}

func TestMIMEAlternative(t *testing.T) {
	from, err := ParseAddress("Acme <no-reply@acme.test>")
	if err != nil {
		t.Fatal(err)
	}
	want := Message{Subject: "Welcome, Zoë", Text: "Hello Zoë\nhttps://work.acme.test/invite#oli_example", HTML: "<p>Hello Zoë</p>"}
	raw, err := encode(from, "zoe@example.test", want, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subject, err := new(mime.WordDecoder).DecodeHeader(m.Header.Get("Subject"))
	if err != nil || subject != want.Subject {
		t.Fatalf("subject %q, %v", subject, err)
	}
	typ, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil || typ != "multipart/alternative" {
		t.Fatalf("content type %q, %v", typ, err)
	}
	parts := multipart.NewReader(m.Body, params["boundary"])
	for i, body := range []string{want.Text, want.HTML} {
		p, err := parts.NextRawPart()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(quotedprintable.NewReader(p))
		if err != nil || string(got) != crlf(body) {
			t.Fatalf("part %d: %q, %v", i, got, err)
		}
	}
	if _, err := parts.NextPart(); err != io.EOF {
		t.Fatalf("extra part: %v", err)
	}
	if m.Header.Get("Auto-Submitted") != "auto-generated" {
		t.Fatal("missing auto-reply protection")
	}
}

func TestHeaderInjectionRejected(t *testing.T) {
	for _, address := range []string{"x@example.test\r\nBcc: y@example.test", "a@example.test, b@example.test", "not-an-address", "Name <a@example.test>"} {
		if _, err := ParseRecipient(address); err == nil {
			t.Errorf("recipient %q accepted", address)
		}
	}
	from, err := ParseAddress("no-reply@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encode(from, "maya@example.test", Message{Subject: "hello\r\nBcc: a@example.test"}, time.Now()); err == nil {
		t.Fatal("subject injection accepted")
	}
}

func TestRetryDelayAndErrorBound(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: 30 * time.Second, 2: time.Minute, 3: 2 * time.Minute, 25: time.Hour} {
		if got := RetryDelay(30*time.Second, attempt); got != want {
			t.Errorf("attempt %d: %v, want %v", attempt, got, want)
		}
	}
	if got := truncate(strings.Repeat("é", 600)); len(got) > maxErrorLength || !utf8.ValidString(got) {
		t.Fatal("error truncation broke UTF-8 or length limit")
	}
}

func TestLOGINRequiresSecureConnection(t *testing.T) {
	auth := &loginAuth{username: "mailer", password: strings.Repeat("test-only", 2), host: "smtp.example.test"}
	if _, _, err := auth.Start(&smtp.ServerInfo{Name: "smtp.example.test", TLS: false}); err == nil {
		t.Fatal("LOGIN accepted a plaintext connection to a remote host")
	}
	if _, _, err := auth.Start(&smtp.ServerInfo{Name: "other.example.test", TLS: true}); err == nil {
		t.Fatal("LOGIN accepted the wrong server name")
	}
	name, _, err := auth.Start(&smtp.ServerInfo{Name: "smtp.example.test", TLS: true})
	if err != nil || name != "LOGIN" {
		t.Fatalf("secure LOGIN: %q, %v", name, err)
	}
	for _, challenge := range []string{"Username:", "Password:"} {
		if response, err := auth.Next([]byte(challenge), true); err != nil || len(response) == 0 {
			t.Fatalf("challenge %q: %v", challenge, err)
		}
	}
	if _, err := auth.Next([]byte("unexpected"), true); err == nil {
		t.Fatal("unexpected LOGIN challenge accepted")
	}
}
