package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// dockerEnv are the environment variables passed to the Docker CLI. Nothing
// else of the agent's environment (in particular ORGLANE_AGENT_SECRET and
// COMPOSE_* overrides) reaches Docker or Compose interpolation.
var dockerEnv = []string{"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR", "DOCKER_HOST", "DOCKER_CONFIG", "DOCKER_CONTEXT", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"}

// runner runs the Docker CLI. The agent builds every argument list itself
// from its configuration, validated instance keys and the reviewed package;
// nothing a client sends becomes a command (ADR 0019).
type runner interface {
	// run returns standard output (at most maxCommandOutput bytes, else
	// errOutputTooLarge) and the end of standard error.
	run(ctx context.Context, stdin []byte, args ...string) (stdout, stderr []byte, err error)
	// tail keeps only the last limit bytes of standard output (logs) and
	// reports whether earlier output was dropped; see tailBuffer.text.
	tail(ctx context.Context, limit int, args ...string) (stdout []byte, truncated bool, stderr []byte, err error)
	// pipe streams standard input from stdin (nil for none) and standard
	// output to stdout without a bound (database dumps and restores).
	pipe(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) (stderr []byte, err error)
	stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error)
}

// dockerCLI runs the docker binary.
type dockerCLI struct {
	bin string
	env []string
}

func newDockerCLI(bin string) *dockerCLI {
	var env []string
	for _, name := range dockerEnv {
		if v, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+v)
		}
	}
	return &dockerCLI{bin: bin, env: env}
}

func (d *dockerCLI) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, d.bin, args...) //nolint:gosec // fixed binary; arguments are built by the agent
	cmd.Env = d.env
	cmd.WaitDelay = 10 * time.Second
	return cmd
}

// exec runs a command, writing standard output to stdout, and returns the
// end of standard error. Output is drained to its end, never buffered
// without bound.
func (d *dockerCLI) exec(ctx context.Context, stdin io.Reader, stdout io.Writer, args []string) ([]byte, error) {
	cmd := d.command(ctx, args)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	stderr := newTailBuffer(maxStderr)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	text, _ := stderr.text()
	return text, err
}

func (d *dockerCLI) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	out := &capBuffer{limit: maxCommandOutput}
	var in io.Reader
	if stdin != nil {
		in = bytes.NewReader(stdin)
	}
	stderr, err := d.exec(ctx, in, out, args)
	if err == nil && out.overflow {
		err = errOutputTooLarge
	}
	return out.buf.Bytes(), stderr, err
}

func (d *dockerCLI) tail(ctx context.Context, limit int, args ...string) ([]byte, bool, []byte, error) {
	out := newTailBuffer(limit)
	stderr, err := d.exec(ctx, nil, out, args)
	text, truncated := out.text()
	return text, truncated, stderr, err
}

func (d *dockerCLI) pipe(ctx context.Context, stdin io.Reader, stdout io.Writer, args ...string) ([]byte, error) {
	if stdout == nil {
		stdout = io.Discard
	}
	return d.exec(ctx, stdin, stdout, args)
}

func (d *dockerCLI) stream(ctx context.Context, args ...string) (io.ReadCloser, func() error, error) {
	cmd := d.command(ctx, args)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	return out, cmd.Wait, nil
}

// commandError is a failed Docker command. Its stderr is redacted and
// truncated; it is logged, never sent to the core.
type commandError struct {
	args   string
	err    error
	stderr string
}

func (e *commandError) Error() string {
	return fmt.Sprintf("docker %s: %v: %s", e.args, e.err, e.stderr)
}

func (e *commandError) Unwrap() error { return e.err }

// compose runs Compose commands for one instance project.
type compose struct {
	r       runner
	project string // <compose project>-<key>
	dir     string // the instance directory
	secrets []string
}

// file names in the instance directory.
const (
	composeFile = "compose.yaml"
	envFile     = ".env"
)

// args returns the base arguments: an explicit -f keeps a stray
// compose.override.yaml from being loaded.
func (c compose) args(file, env string, sub ...string) []string {
	return append([]string{
		"compose", "--project-name", c.project, "--project-directory", c.dir,
		"-f", file, "--env-file", env,
	}, sub...)
}

func (c compose) do(ctx context.Context, sub ...string) ([]byte, error) {
	return c.doFiles(ctx, filepath.Join(c.dir, composeFile), filepath.Join(c.dir, envFile), sub...)
}

func (c compose) doFiles(ctx context.Context, file, env string, sub ...string) ([]byte, error) {
	out, stderr, err := c.r.run(ctx, nil, c.args(file, env, sub...)...)
	if err != nil {
		return nil, &commandError{args: "compose " + strings.Join(sub, " "), err: err, stderr: truncate(redact(stderr, c.secrets))}
	}
	return out, nil
}

// config normalizes a project in the staging files.
func (c compose) config(ctx context.Context, file, env string) ([]byte, error) {
	return c.doFiles(ctx, file, env, "config", "--format", "json")
}

func (c compose) up(ctx context.Context, timeout time.Duration) error {
	secs := max(int(timeout.Seconds()), 1)
	_, err := c.do(ctx, "up", "--detach", "--remove-orphans", "--wait", "--wait-timeout", strconv.Itoa(secs))
	return err
}

func (c compose) stop(ctx context.Context, services ...string) error {
	_, err := c.do(ctx, append([]string{"stop"}, services...)...)
	return err
}

// pull pulls the images of a staged project that are not present yet.
// Images are pinned by digest, so a present image is the right one.
func (c compose) pull(ctx context.Context, file, env string) error {
	_, err := c.doFiles(ctx, file, env, "pull", "--quiet", "--policy", "missing")
	return err
}

// start starts the project's containers without waiting (upgrade
// switching; verifying waits with up).
func (c compose) start(ctx context.Context) error {
	_, err := c.do(ctx, "up", "--detach", "--remove-orphans")
	return err
}

func (c compose) down(ctx context.Context) error {
	_, err := c.do(ctx, "down", "--remove-orphans")
	return err
}

func (c compose) exec(ctx context.Context, service string, argv []string) error {
	_, err := c.do(ctx, append([]string{"exec", "-T", service}, argv...)...)
	return err
}

// logs returns the end of a service's logs, at most limit bytes, and
// whether earlier output was dropped (at a line boundary).
func (c compose) logs(ctx context.Context, service string, tail int, since string, limit int) ([]byte, bool, error) {
	args := []string{"logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(tail)}
	if since != "" {
		args = append(args, "--since", since)
	}
	args = c.args(filepath.Join(c.dir, composeFile), filepath.Join(c.dir, envFile), append(args, service)...)
	out, truncated, stderr, err := c.r.tail(ctx, limit, args...)
	if err != nil {
		return nil, false, &commandError{args: "compose logs", err: err, stderr: truncate(redact(stderr, c.secrets))}
	}
	return out, truncated, nil
}

// ps returns the state of the project's containers.
func (c compose) ps(ctx context.Context) ([]agentapi.ServiceStatus, error) {
	out, err := c.do(ctx, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	type container struct {
		Service string `json:"Service"`
		State   string `json:"State"`
		Health  string `json:"Health"`
	}
	var list []container
	trimmed := bytes.TrimSpace(out)
	if bytes.HasPrefix(trimmed, []byte("[")) {
		err = json.Unmarshal(trimmed, &list)
	} else {
		// One JSON object per line (Compose 2.21 and later).
		dec := json.NewDecoder(bytes.NewReader(trimmed))
		for dec.More() {
			var ct container
			if err = dec.Decode(&ct); err != nil {
				break
			}
			list = append(list, ct)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("decode compose ps: %w", err)
	}
	out2 := make([]agentapi.ServiceStatus, 0, len(list))
	for _, ct := range list {
		out2 = append(out2, agentapi.ServiceStatus{Name: ct.Service, State: ct.State, Health: ct.Health})
	}
	slices.SortFunc(out2, func(a, b agentapi.ServiceStatus) int { return strings.Compare(a.Name, b.Name) })
	return out2, nil
}

// postgres runs SQL in the bundle's PostgreSQL container with docker exec as
// the bundle's user over its local socket: the agent needs no password and
// no network (docs/specs/agent.md "Per-service database").
type postgres struct {
	r       runner
	project string // the bundle's Compose project
	service string
	user    string
	// timeout bounds the wait for the server to accept connections, wait is
	// the pause between attempts; zero means postgresReadyTimeout and
	// postgresRetryWait.
	timeout, wait time.Duration
}

// The server may not accept connections yet, or for a moment: the image
// initializes a new data directory with a temporary server on the socket
// and restarts it, and the bundle may restart its container. The agent waits
// that long for it before the step fails.
const (
	postgresReadyTimeout = 2 * time.Minute
	postgresRetryWait    = 500 * time.Millisecond
)

// psql's exit status when the connection to the server failed or went bad;
// 3 is an error in the script (ON_ERROR_STOP).
const psqlConnectionFailed = 2

var (
	errNoPostgres       = errors.New("the bundle's PostgreSQL container is not running")
	errPostgresNotReady = errors.New("the bundle's PostgreSQL does not accept connections")
)

func (p postgres) container(ctx context.Context) (string, error) {
	out, stderr, err := p.r.run(ctx, nil, "ps", "--quiet", "--no-trunc",
		"--filter", "label=com.docker.compose.project="+p.project,
		"--filter", "label=com.docker.compose.service="+p.service,
		"--filter", "status=running")
	if err != nil {
		return "", &commandError{args: "ps", err: err, stderr: truncate(stderr)}
	}
	ids := strings.Fields(string(out))
	if len(ids) != 1 {
		return "", errNoPostgres
	}
	return ids[0], nil
}

// accepting reports whether the server in container id accepts TCP
// connections (pg_isready exits 0; it needs no valid role). The probe goes
// over TCP on purpose: while the image initializes a new data directory its
// temporary server listens only on the Unix socket, so a socket probe would
// pass before the restart that installs the real server (#78).
func (p postgres) accepting(ctx context.Context, id string) bool {
	_, _, err := p.r.run(ctx, nil, "exec", id, "pg_isready", "-q", "-h", "127.0.0.1", "-U", p.user, "-d", "postgres")
	return err == nil
}

// deadline returns the time the wait for the server ends.
func (p postgres) deadline() time.Time {
	if p.timeout > 0 {
		return time.Now().Add(p.timeout)
	}
	return time.Now().Add(postgresReadyTimeout)
}

// pause waits before the next attempt; it fails with cause once the
// deadline has passed or ctx ended.
func (p postgres) pause(ctx context.Context, deadline time.Time, cause error) error {
	if !time.Now().Before(deadline) {
		return cause
	}
	wait := p.wait
	if wait <= 0 {
		wait = postgresRetryWait
	}
	t := time.NewTimer(min(wait, time.Until(deadline)))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return errors.Join(cause, context.Cause(ctx))
	case <-t.C:
		return nil
	}
}

// ready waits until the container runs and its server accepts connections,
// and returns the container. Other errors fail at once. Its Docker probes are
// bounded by deadline, so a stalled daemon or container command cannot hold
// the step past its readiness budget; a probe that succeeds only after the
// deadline is not accepted.
func (p postgres) ready(ctx context.Context, deadline time.Time) (string, error) {
	probe, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var cause error
	for {
		id, err := p.container(probe)
		switch {
		case err != nil && !errors.Is(err, errNoPostgres):
			if probe.Err() != nil {
				return "", errors.Join(err, context.Cause(probe))
			}
			return "", err
		case err != nil:
			cause = err
		case !p.accepting(probe, id):
			cause = errPostgresNotReady
		case probe.Err() == nil:
			return id, nil
		default:
			cause = errPostgresNotReady // succeeded too late
		}
		if err := p.pause(probe, deadline, cause); err != nil {
			return "", err
		}
	}
}

// stillReady reports whether container id is still the running container and
// its server accepts connections. Its Docker probes are bounded by deadline
// like those of ready.
func (p postgres) stillReady(ctx context.Context, deadline time.Time, id string) (same, ready bool) {
	probe, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	cur, err := p.container(probe)
	if err != nil || cur != id {
		return false, false
	}
	accepting := p.accepting(probe, id)
	return true, accepting && probe.Err() == nil
}

// script runs psql with args on SQL from standard input once the server
// accepts connections. A connection that fails or breaks while the server
// does not accept connections (it is starting or restarting), or because
// its container stopped, is tried again until the deadline; every script
// the agent runs is safe to repeat. An error in the SQL fails at once, and
// so does a connection the ready server refuses twice (a role or database
// that does not exist). secrets are redacted from errors.
func (p postgres) script(ctx context.Context, args []string, sql string, secrets []string) ([]byte, error) {
	deadline := p.deadline()
	refused := 0
	for {
		id, err := p.ready(ctx, deadline)
		if err != nil {
			return nil, err
		}
		out, stderr, err := p.r.run(ctx, []byte(sql), append([]string{"exec", "-i", id, "psql"}, args...)...)
		if err == nil {
			return out, nil
		}
		cerr := &commandError{args: "exec psql", err: err, stderr: truncate(redact(stderr, secrets))}
		if ctx.Err() != nil {
			return nil, cerr
		}
		// A container that stopped or was replaced under the command is
		// waited for; otherwise only a connection failure is. These probes
		// are bounded by the readiness deadline too.
		if same, accepts := p.stillReady(ctx, deadline, id); same {
			if !exitedWith(err, psqlConnectionFailed) {
				return nil, cerr
			}
			if accepts {
				if refused++; refused == 2 {
					return nil, cerr
				}
				continue // the server may have come up just after the attempt
			}
		}
		if err := p.pause(ctx, deadline, cerr); err != nil {
			return nil, err
		}
	}
}

// exitedWith reports whether err is a command that exited with code.
func exitedWith(err error, code int) bool {
	var ee interface{ ExitCode() int }
	return errors.As(err, &ee) && ee.ExitCode() == code
}

// exec runs SQL from standard input, never on the command line. secrets are
// redacted from errors.
func (p postgres) exec(ctx context.Context, sql string, secrets []string) error {
	_, err := p.script(ctx, []string{"-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", p.user, "-d", "postgres", "-f", "-"}, sql, secrets)
	return err
}

// query runs one SQL statement from standard input and returns its
// unaligned, tuples-only output.
func (p postgres) query(ctx context.Context, sql string) (string, error) {
	out, err := p.script(ctx, []string{"-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-U", p.user, "-d", "postgres", "-f", "-"}, sql, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// dump writes a custom-format pg_dump of a database to w. It waits for the
// server but is not repeated: w may hold part of the output.
func (p postgres) dump(ctx context.Context, database string, w io.Writer) error {
	id, err := p.ready(ctx, p.deadline())
	if err != nil {
		return err
	}
	stderr, err := p.r.pipe(ctx, nil, w, "exec", id, "pg_dump", "-U", p.user, "-Fc", "-d", database)
	if err != nil {
		return &commandError{args: "exec pg_dump", err: err, stderr: truncate(stderr)}
	}
	return nil
}

// restore runs pg_restore of a custom-format dump from r into an empty
// database, as role, so every restored object is owned by it. It waits for
// the server but is not repeated: r has been read.
func (p postgres) restore(ctx context.Context, database, role string, r io.Reader) error {
	id, err := p.ready(ctx, p.deadline())
	if err != nil {
		return err
	}
	stderr, err := p.r.pipe(ctx, r, nil, "exec", "-i", id, "pg_restore", "-U", p.user, "-d", database,
		"--no-owner", "--role="+role, "--exit-on-error", "--single-transaction")
	if err != nil {
		return &commandError{args: "exec pg_restore", err: err, stderr: truncate(stderr)}
	}
	return nil
}

// events streams the Compose projects of container events (die, destroy,
// health_status) until ctx ends or the stream fails.
func events(ctx context.Context, r runner, fn func(project string)) error {
	out, wait, err := r.stream(ctx, "events", "--format", "{{json .}}",
		"--filter", "type=container", "--filter", "label=com.docker.compose.project")
	if err != nil {
		return err
	}
	s := bufio.NewScanner(out)
	s.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for s.Scan() {
		var ev struct {
			Action string `json:"Action"`
			Actor  struct {
				Attributes map[string]string `json:"Attributes"`
			} `json:"Actor"`
		}
		if json.Unmarshal(s.Bytes(), &ev) != nil {
			continue
		}
		if ev.Action == "die" || ev.Action == "destroy" || strings.HasPrefix(ev.Action, "health_status") {
			fn(ev.Actor.Attributes["com.docker.compose.project"])
		}
	}
	_ = out.Close()
	return errors.Join(s.Err(), wait())
}

func truncate(b []byte) string {
	const limit = 2048
	if len(b) > limit {
		b = append(b[:limit:limit], "..."...)
	}
	return strings.TrimSpace(string(b))
}
