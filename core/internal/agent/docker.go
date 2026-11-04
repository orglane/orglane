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
	run(ctx context.Context, stdin []byte, args ...string) (stdout, stderr []byte, err error)
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

func (d *dockerCLI) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	cmd := d.command(ctx, args)
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	return stdout.Bytes(), stderr.Bytes(), err
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

func (c compose) stop(ctx context.Context) error {
	_, err := c.do(ctx, "stop")
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

func (c compose) logs(ctx context.Context, service string, tail int, since string) ([]byte, error) {
	args := []string{"logs", "--no-color", "--timestamps", "--tail", strconv.Itoa(tail)}
	if since != "" {
		args = append(args, "--since", since)
	}
	return c.do(ctx, append(args, service)...)
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
}

var errNoPostgres = errors.New("the bundle's PostgreSQL container is not running")

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

// exec runs SQL from standard input, never on the command line. secrets are
// redacted from errors.
func (p postgres) exec(ctx context.Context, sql string, secrets []string) error {
	id, err := p.container(ctx)
	if err != nil {
		return err
	}
	_, stderr, err := p.r.run(ctx, []byte(sql), "exec", "-i", id,
		"psql", "-X", "-q", "-v", "ON_ERROR_STOP=1", "-U", p.user, "-d", "postgres", "-f", "-")
	if err != nil {
		return &commandError{args: "exec psql", err: err, stderr: truncate(redact(stderr, secrets))}
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
