// Package agentclient is the core's client of the agent (docs/specs/agent.md
// "Transport and authentication"). It signs every request, refuses every
// response without a valid signature, proves the peer's key on each new
// connection before it sends anything else, and sends mutating requests
// only to an agent of the same version.
package agentclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/config"
)

// Errors of the client. Agent answers other than 2xx are *ProblemError.
var (
	// ErrUnavailable: the agent cannot be reached (not running, socket
	// missing). Retryable (agent_unavailable).
	ErrUnavailable = errors.New("agent_unavailable: the agent is not reachable")
	// ErrUntrusted: an answer without a valid signature, from a process
	// that does not hold the key or after a secret mismatch (agent_untrusted).
	ErrUntrusted = errors.New("agent_untrusted: the agent's answer is not signed with the shared key")
	// ErrVersionMismatch: the agent runs another Orglane version
	// (agent_version_mismatch). Mutating requests are not sent.
	ErrVersionMismatch = errors.New("agent_version_mismatch: the agent runs another Orglane version")
	// ErrProtocol: the agent speaks another protocol version.
	ErrProtocol = errors.New("agent_protocol_mismatch: the agent speaks another protocol version")
)

// ProblemError is a problem details answer of the agent.
type ProblemError struct {
	agentapi.Problem
}

func (e *ProblemError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("agent: %d %s: %s", e.Status, e.Code, e.Detail)
	}
	return fmt.Sprintf("agent: %d %s", e.Status, e.Code)
}

// maxResponseBytes bounds answers (logs are at most 1 MiB).
const maxResponseBytes = agentapi.MaxLogBytes + 64*1024

// handshakeTimeout bounds the key proof on a new connection.
const handshakeTimeout = 10 * time.Second

// Client talks to the agent over its Unix socket.
type Client struct {
	key     agentapi.Key
	version string
	http    *http.Client
	now     func() time.Time
}

// New returns a client for the socket. version is the core's version.
func New(socket, secret, version string) (*Client, error) {
	key, err := agentapi.DeriveKey(secret)
	if err != nil {
		return nil, err
	}
	c := &Client{key: key, version: version, now: time.Now}
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	c.http = &http.Client{
		Timeout: 2 * time.Minute,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, "unix", socket)
				if err != nil {
					return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
				}
				pc, err := c.handshake(ctx, conn)
				if err != nil {
					_ = conn.Close()
					return nil, err
				}
				return pc, nil
			},
			MaxIdleConns:          4,
			IdleConnTimeout:       time.Minute,
			ResponseHeaderTimeout: 2 * time.Minute,
			DisableCompression:    true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return c, nil
}

// FromConfig returns a client for the core's agent settings, or nil when
// managed services are disabled (no socket or no secret).
func FromConfig(cfg config.Agent, version string) (*Client, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	return New(cfg.Socket, cfg.Secret, version)
}

// baseURL is the URL of every request; the host is not used.
const baseURL = "http://orglane-agent"

// handshake sends a signed GET /v1/health on a new connection and checks the
// signed answer, so the peer has proven the key before the connection
// carries anything else (requests with secrets in particular).
func (c *Client) handshake(ctx context.Context, conn net.Conn) (net.Conn, error) {
	deadline := time.Now().Add(handshakeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+agentapi.PathHealth, http.NoBody)
	if err != nil {
		return nil, err
	}
	mac := agentapi.SignRequest(c.key, req, nil, c.now())
	if err := req.Write(conn); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUntrusted, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	_ = resp.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUntrusted, err)
	}
	if !agentapi.VerifyResponse(c.key, mac, resp.StatusCode, resp.Header, body) || resp.StatusCode != http.StatusOK {
		return nil, ErrUntrusted
	}
	if resp.Close {
		return nil, fmt.Errorf("%w: the agent closed the connection", ErrUnavailable)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return &provenConn{Conn: conn, r: br}, nil
}

// provenConn is a connection whose peer proved the key. Reads go through the
// handshake's buffer, which holds nothing unless the peer misbehaved.
type provenConn struct {
	net.Conn
	r *bufio.Reader
}

func (p *provenConn) Read(b []byte) (int, error) { return p.r.Read(b) }

// do sends a signed request and returns the verified answer.
func (c *Client) do(ctx context.Context, method, target string, body []byte) (int, []byte, error) {
	var rd io.Reader = http.NoBody
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, baseURL+target, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	mac := agentapi.SignRequest(c.key, req, body, c.now())
	resp, err := c.http.Do(req)
	if err != nil {
		if errors.Is(err, ErrUntrusted) || errors.Is(err, ErrUnavailable) {
			return 0, nil, err
		}
		return 0, nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrUnavailable, err)
	}
	if len(data) > maxResponseBytes {
		return 0, nil, fmt.Errorf("%w: answer too large", ErrUntrusted)
	}
	if !agentapi.VerifyResponse(c.key, mac, resp.StatusCode, resp.Header, data) {
		return 0, nil, ErrUntrusted
	}
	if resp.StatusCode >= 300 {
		var p agentapi.Problem
		if err := json.Unmarshal(data, &p); err != nil || p.Code == "" {
			p = agentapi.Problem{Status: resp.StatusCode, Code: agentapi.CodeInternal, Title: http.StatusText(resp.StatusCode)}
		}
		p.Status = resp.StatusCode
		return resp.StatusCode, data, &ProblemError{Problem: p}
	}
	return resp.StatusCode, data, nil
}

func (c *Client) getJSON(ctx context.Context, target string, v any) error {
	_, data, err := c.do(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("agent: decode answer: %w", err)
	}
	return nil
}

// Health returns the agent's version, boot ID, packages and host resources.
func (c *Client) Health(ctx context.Context) (*agentapi.Health, error) {
	var h agentapi.Health
	if err := c.getJSON(ctx, agentapi.PathHealth, &h); err != nil {
		return nil, err
	}
	if h.Protocol != agentapi.ProtocolVersion {
		return &h, ErrProtocol
	}
	return &h, nil
}

// CompatibleVersions reports whether a core and an agent version may work
// together: equal, or either is a development build.
func CompatibleVersions(core, agent string) bool {
	return core == agent || core == "dev" || agent == "dev"
}

// checkVersion refuses mutating requests to an agent of another version.
func (c *Client) checkVersion(ctx context.Context) error {
	h, err := c.Health(ctx)
	if err != nil {
		return err
	}
	if !CompatibleVersions(c.version, h.Version) {
		return fmt.Errorf("%w: core %s, agent %s", ErrVersionMismatch, c.version, h.Version)
	}
	return nil
}

// Instances returns the status of every instance the agent knows.
func (c *Client) Instances(ctx context.Context) ([]agentapi.InstanceStatus, error) {
	var l agentapi.InstanceList
	if err := c.getJSON(ctx, agentapi.PathInstances, &l); err != nil {
		return nil, err
	}
	return l.Instances, nil
}

// Instance returns the status of one instance.
func (c *Client) Instance(ctx context.Context, key string) (*agentapi.InstanceStatus, error) {
	var s agentapi.InstanceStatus
	if err := c.getJSON(ctx, agentapi.InstancePath(key), &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Apply pushes a desired spec. accepted is true for a newly accepted spec
// (202) and false for a retry of the accepted generation (200); either way
// the operation is the one that applies it.
func (c *Client) Apply(ctx context.Context, key string, spec *agentapi.Spec) (op *agentapi.Operation, accepted bool, err error) {
	if !agentapi.KeyPattern.MatchString(key) {
		return nil, false, fmt.Errorf("agent: invalid instance key %q", key)
	}
	if fields := spec.Check(); len(fields) > 0 {
		return nil, false, fmt.Errorf("agent: invalid spec fields %v", fields)
	}
	if err := c.checkVersion(ctx); err != nil {
		return nil, false, err
	}
	body, err := json.Marshal(spec)
	if err != nil {
		return nil, false, err
	}
	status, data, err := c.do(ctx, http.MethodPut, agentapi.InstancePath(key), body)
	if err != nil {
		return nil, false, err
	}
	var o agentapi.Operation
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, false, fmt.Errorf("agent: decode operation: %w", err)
	}
	return &o, status == http.StatusAccepted, nil
}

// Operation returns an operation's state.
func (c *Client) Operation(ctx context.Context, id uuid.UUID) (*agentapi.Operation, error) {
	var o agentapi.Operation
	if err := c.getJSON(ctx, agentapi.OperationPath(id), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// LogOptions select service logs.
type LogOptions struct {
	Service string
	Tail    int           // lines, 1..1000; 0 means the agent's default
	Since   time.Duration // 0 means no limit
}

// Logs returns bounded, redacted logs of one service.
func (c *Client) Logs(ctx context.Context, key string, o LogOptions) (string, error) {
	q := url.Values{"service": {o.Service}}
	if o.Tail > 0 {
		q.Set("tail", strconv.Itoa(o.Tail))
	}
	if o.Since > 0 {
		q.Set("since", o.Since.String())
	}
	_, data, err := c.do(ctx, http.MethodGet, agentapi.InstanceLogsPath(key)+"?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// Close releases idle connections.
func (c *Client) Close() { c.http.CloseIdleConnections() }
