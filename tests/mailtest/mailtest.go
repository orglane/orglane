//go:build integration

// Package mailtest runs Mailpit, a real SMTP server with an HTTP API, in a
// container for integration tests of system email (testcontainers-go).
// Tests using it carry the "integration" build tag and need Docker.
package mailtest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the Mailpit image used by tests and deploy/dev/compose.yaml.
const Image = "axllent/mailpit:v1.29.7"

// Mailpit is a running Mailpit server.
type Mailpit struct {
	// SMTPHost and SMTPPort accept mail without TLS or authentication.
	SMTPHost string
	SMTPPort int
	// API is the base URL of the HTTP API, e.g. http://127.0.0.1:32768.
	API string
}

// Start runs a fresh Mailpit for the test; it is removed when the test
// ends.
func Start(t testing.TB) *Mailpit {
	t.Helper()
	ctx := context.Background()
	ctr, err := testcontainers.Run(ctx, Image,
		testcontainers.WithExposedPorts("1025/tcp", "8025/tcp"),
		testcontainers.WithWaitStrategy(
			wait.ForHTTP("/livez").WithPort("8025/tcp").WithStartupTimeout(60*time.Second),
			wait.ForListeningPort("1025/tcp"),
		),
	)
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			t.Logf("mailtest: terminate container: %v", err)
		}
	})
	if err != nil {
		t.Fatalf("mailtest: start %s (is Docker running?): %v", Image, err)
	}
	host, err := ctr.Host(ctx)
	if err != nil {
		t.Fatal(err)
	}
	smtpPort, err := ctr.MappedPort(ctx, "1025/tcp")
	if err != nil {
		t.Fatal(err)
	}
	apiPort, err := ctr.MappedPort(ctx, "8025/tcp")
	if err != nil {
		t.Fatal(err)
	}
	return &Mailpit{SMTPHost: host, SMTPPort: int(smtpPort.Num()), API: fmt.Sprintf("http://%s:%s", host, apiPort.Port())}
}

// Message is a received email.
type Message struct {
	ID      string `json:"ID"`
	Subject string `json:"Subject"`
	From    struct {
		Name    string `json:"Name"`
		Address string `json:"Address"`
	} `json:"From"`
	To []struct {
		Address string `json:"Address"`
	} `json:"To"`
	Text string `json:"Text"`
	HTML string `json:"HTML"`
}

// Messages returns the messages received for to, newest first, with their
// bodies.
func (m *Mailpit) Messages(t testing.TB, to string) []Message {
	t.Helper()
	var list struct {
		Messages []Message `json:"messages"`
	}
	m.get(t, "/api/v1/search?query="+url.QueryEscape("to:"+to), &list)
	out := make([]Message, 0, len(list.Messages))
	for _, s := range list.Messages {
		var full Message
		m.get(t, "/api/v1/message/"+url.PathEscape(s.ID), &full)
		out = append(out, full)
	}
	return out
}

// Wait waits until n messages were received for to and returns them,
// newest first.
func (m *Mailpit) Wait(t testing.TB, to string, n int, timeout time.Duration) []Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		msgs := m.Messages(t, to)
		if len(msgs) >= n {
			return msgs
		}
		if time.Now().After(deadline) {
			t.Fatalf("mailtest: %d of %d messages for %s within %s", len(msgs), n, to, timeout)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (m *Mailpit) get(t testing.TB, path string, v any) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, m.API+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("mailtest: GET %s: %v", path, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("mailtest: GET %s = %d", path, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(v); err != nil {
		t.Fatalf("mailtest: decode %s: %v", path, err)
	}
}

// Link returns the first URL in text that starts with prefix.
func Link(text, prefix string) string {
	i := strings.Index(text, prefix)
	if i < 0 {
		return ""
	}
	end := strings.IndexAny(text[i:], " \r\n\"<>")
	if end < 0 {
		return text[i:]
	}
	return text[i : i+end]
}
