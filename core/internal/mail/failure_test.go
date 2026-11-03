package mail

import (
	"bufio"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/config"
)

func TestUntrustedDiagnosticsAreDiscarded(t *testing.T) {
	token := "oli_" + strings.Repeat("A", 43)
	smtp := SMTP{Username: "canary-user", Password: strings.Repeat("canary-secret", 2)}
	raw := errors.New("550 blocked URL /invite#" + token + " " + smtp.Username + " " + smtp.Password)
	for _, failure := range []error{raw, &smtpFailure{stage: "connection", address: smtp.Username + "." + smtp.Password + ".example.test"}} {
		err := safeDeliveryFailure(failure, smtp)
		assertNoDiagnosticSecrets(t, err.Error(), token, smtp)
		if errors.Unwrap(err) != nil {
			t.Fatal("sanitized error retains the untrusted cause")
		}
	}
}

func assertNoDiagnosticSecrets(t *testing.T, text, token string, smtp SMTP) {
	t.Helper()
	for _, secret := range []string{token, smtp.Username, smtp.Password, base64.StdEncoding.EncodeToString([]byte(smtp.Password)), base64.StdEncoding.EncodeToString([]byte("\x00" + smtp.Username + "\x00" + smtp.Password))} {
		if secret != "" && strings.Contains(text, secret) {
			t.Fatal("SMTP diagnostic contains a bearer token or configured credential")
		}
	}
}

// A unit-only adversarial protocol fixture: Mailpit intentionally does not
// echo arbitrary message content in DATA errors. Lifecycle integration still
// uses real Mailpit/Postgres; this tests the hostile wire response boundary.
func TestSMTPDataRejectionCannotEchoSecrets(t *testing.T) {
	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		t.Fatal(err)
	}
	smtp := SMTP{Host: host, Port: n, TLS: config.SMTPNoTLS, Username: "canary-user", Password: strings.Repeat("canary-secret", 2), From: "no-reply@example.test"}
	token := "oli_" + strings.Repeat("B", 43)
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
		r := bufio.NewReader(conn)
		_, _ = io.WriteString(conn, "220 local SMTP unit fixture\r\n")
		for {
			line, err := r.ReadString('\n')
			if errors.Is(err, io.EOF) {
				done <- nil
				return
			}
			if err != nil {
				done <- err
				return
			}
			switch {
			case strings.HasPrefix(line, "EHLO"):
				_, _ = io.WriteString(conn, "250-localhost\r\n250 AUTH PLAIN\r\n")
			case strings.HasPrefix(line, "AUTH"):
				_, _ = io.WriteString(conn, "235 authenticated\r\n")
			case strings.HasPrefix(line, "DATA"):
				_, _ = io.WriteString(conn, "354 send message\r\n")
				for {
					line, err = r.ReadString('\n')
					if err != nil {
						done <- err
						return
					}
					if line == ".\r\n" {
						break
					}
				}
				_, _ = fmt.Fprintf(conn, "550 blocked /invite#%s user=%s password=%s\r\n", token, smtp.Username, smtp.Password)
			case strings.HasPrefix(line, "QUIT"):
				_, _ = io.WriteString(conn, "221 bye\r\n")
				done <- nil
				return
			default:
				_, _ = io.WriteString(conn, "250 ok\r\n")
			}
		}
	}()
	err = send(t.Context(), smtp, "localhost", "maya@example.test", Message{Subject: "Invite", Text: "/invite#" + token, HTML: "<p>Invite</p>"})
	if err == nil {
		t.Fatal("hostile DATA rejection was accepted")
	}
	assertNoDiagnosticSecrets(t, err.Error(), token, smtp)
	assertNoDiagnosticSecrets(t, safeDeliveryFailure(err, smtp).Error(), token, smtp)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
