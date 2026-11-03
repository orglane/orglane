package mail

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/orglane/orglane/core/internal/config"
)

// SMTP is an SMTP server and sender for system email.
type SMTP struct {
	Host string
	Port int
	// TLS is config.SMTPStartTLS, config.SMTPImplicitTLS or config.SMTPNoTLS.
	TLS      string
	Username string
	Password string
	// From is the sender, e.g. "Acme <no-reply@acme.com>".
	From string
}

// sendTimeout bounds one delivery attempt, from connecting to QUIT.
const sendTimeout = 30 * time.Second

// send delivers one message. helo is the name the core greets the server
// with (the workspace host). Credentials are only sent over TLS, or to a
// server on localhost (net/smtp refuses otherwise).
func send(ctx context.Context, s SMTP, helo, to string, m Message) error {
	from, err := ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("the sender %q is not a valid address", s.From)
	}
	body, err := encode(from, to, m, time.Now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()

	addr := net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	tlsConfig := &tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}
	var conn net.Conn
	if s.TLS == config.SMTPImplicitTLS {
		conn, err = (&tls.Dialer{Config: tlsConfig}).DialContext(ctx, "tcp", addr)
	} else {
		var d net.Dialer
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("connect to %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	// Close the connection when ctx ends, so a hanging server cannot block
	// shutdown past the deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		return fmt.Errorf("greeting from %s: %w", addr, err)
	}
	defer func() { _ = c.Close() }()
	if err := c.Hello(helo); err != nil {
		return fmt.Errorf("EHLO: %w", err)
	}
	if s.TLS == config.SMTPStartTLS {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("the server does not offer STARTTLS; choose TLS mode \"tls\" or \"none\"")
		}
		if err := c.StartTLS(tlsConfig); err != nil {
			return fmt.Errorf("STARTTLS: %w", err)
		}
	}
	if s.Username != "" {
		auth, err := chooseAuth(c, s)
		if err != nil {
			return err
		}
		if err := c.Auth(auth); err != nil {
			return fmt.Errorf("authentication failed: %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("MAIL FROM: %w", err)
	}
	if err := c.Rcpt(to); err != nil {
		return fmt.Errorf("RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if _, err := w.Write(body); err != nil {
		return fmt.Errorf("DATA: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("the server rejected the message: %w", err)
	}
	// The message is accepted; a failing QUIT does not change that.
	_ = c.Quit()
	return nil
}

// chooseAuth picks PLAIN, or LOGIN when the server offers only that.
func chooseAuth(c *smtp.Client, s SMTP) (smtp.Auth, error) {
	ok, mechs := c.Extension("AUTH")
	if !ok {
		return nil, errors.New("the server does not offer authentication; clear the username and password, or check the TLS mode")
	}
	fields := strings.Fields(strings.ToUpper(mechs))
	has := func(m string) bool {
		for _, f := range fields {
			if f == m {
				return true
			}
		}
		return false
	}
	switch {
	case has("PLAIN"):
		return smtp.PlainAuth("", s.Username, s.Password, s.Host), nil
	case has("LOGIN"):
		return &loginAuth{username: s.Username, password: s.Password, host: s.Host}, nil
	default:
		return nil, fmt.Errorf("the server offers no supported authentication (PLAIN or LOGIN), only %q", mechs)
	}
}

// loginAuth is the LOGIN mechanism, which some providers offer instead of
// PLAIN. Like smtp.PlainAuth it refuses to send credentials in the clear
// except to localhost.
type loginAuth struct {
	username, password, host string
}

func (a *loginAuth) Start(server *smtp.ServerInfo) (string, []byte, error) {
	if !server.TLS && !isLocalhost(server.Name) {
		return "", nil, errors.New("unencrypted connection")
	}
	if server.Name != a.host {
		return "", nil, errors.New("wrong host name")
	}
	return "LOGIN", nil, nil
}

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch strings.ToLower(strings.TrimSpace(string(fromServer))) {
	case "username:":
		return []byte(a.username), nil
	case "password:":
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("unexpected LOGIN challenge %q", fromServer)
	}
}

func isLocalhost(name string) bool {
	return name == "localhost" || name == "127.0.0.1" || name == "::1"
}
