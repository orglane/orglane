package agent

import (
	"context"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// exitError is a command that exited with a status, like *exec.ExitError.
type exitError int

// Error implements error.
func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }

// ExitCode reports the command's exit status.
func (e exitError) ExitCode() int { return int(e) }

// pgRunner answers the agent's PostgreSQL commands from the test's
// functions: ps (the running container, "" for none), pg_isready and psql.
type pgRunner struct {
	mu        sync.Mutex
	ps        func(n int) string
	ready     func(n int) bool
	psql      func(n int) error
	psN       int
	readyN    int
	psqlN     int
	readyArgs []string
	psqlArgs  []string
}

// run implements runner for the commands of one PostgreSQL step.
func (r *pgRunner) run(_ context.Context, _ []byte, args ...string) ([]byte, []byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case args[0] == "ps":
		r.psN++
		return []byte(r.ps(r.psN)), nil, nil
	case args[0] == "exec" && args[2] == "pg_isready":
		r.readyN++
		r.readyArgs = args
		if r.ready(r.readyN) {
			return nil, nil, nil
		}
		return nil, nil, exitError(2)
	case args[0] == "exec" && args[3] == "psql":
		r.psqlN++
		r.psqlArgs = args
		if err := r.psql(r.psqlN); err != nil {
			return nil, []byte("psql: error: password 'p4ss' refused"), err
		}
		return []byte(" 1\n"), nil, nil
	}
	return nil, nil, errors.New("unexpected command " + strings.Join(args, " "))
}

// tail implements runner; the database step does not tail.
func (r *pgRunner) tail(context.Context, int, ...string) ([]byte, bool, []byte, error) {
	return nil, false, nil, errors.New("unexpected tail")
}

// pipe implements runner; the database step pipes only dumps and restores.
func (r *pgRunner) pipe(_ context.Context, _ io.Reader, _ io.Writer, args ...string) ([]byte, error) {
	return nil, errors.New("unexpected pipe " + strings.Join(args, " "))
}

// stream implements runner; the database step does not stream.
func (r *pgRunner) stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("unexpected stream")
}

// always returns v for every call.
func always[T any](v T) func(int) T { return func(int) T { return v } }

// from returns before for the first n calls, then after.
func from[T any](n int, before, after T) func(int) T {
	return func(i int) T {
		if i <= n {
			return before
		}
		return after
	}
}

// testPostgres returns the runner-backed postgres with short waits.
func testPostgres(r *pgRunner) postgres {
	return postgres{r: r, project: "orglane", service: "postgres", user: "orglane", timeout: 2 * time.Second, wait: time.Millisecond}
}

// TestPostgresWaitsForTheServer covers the waits of the per-service database
// step (#78): a server that is still starting, a container that is not
// running yet and the restart after the image's initialization are waited
// for; the script then runs. The readiness probe must go over TCP, so the
// image's temporary initialization server on the Unix socket is not taken
// for the real one.
func TestPostgresWaitsForTheServer(t *testing.T) {
	for name, tc := range map[string]struct {
		r        *pgRunner
		psqlRuns int
	}{
		"starting": {
			r:        &pgRunner{ps: always("c1"), ready: from(3, false, true), psql: always[error](nil)},
			psqlRuns: 1,
		},
		"container not running yet": {
			r:        &pgRunner{ps: from(2, "", "c1"), ready: always(true), psql: always[error](nil)},
			psqlRuns: 1,
		},
		"restart after the attempt": {
			// The server went away under psql; it accepts again two checks
			// later.
			r:        &pgRunner{ps: always("c1"), ready: func(n int) bool { return n == 1 || n > 3 }, psql: from[error](1, exitError(2), nil)},
			psqlRuns: 2,
		},
		"up just after the attempt": {
			r:        &pgRunner{ps: always("c1"), ready: always(true), psql: from[error](1, exitError(2), nil)},
			psqlRuns: 2,
		},
		"container replaced under the command": {
			// docker exec fails with 1 for a container that is gone.
			r:        &pgRunner{ps: from(1, "c1", "c2"), ready: always(true), psql: from[error](1, exitError(1), nil)},
			psqlRuns: 2,
		},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := testPostgres(tc.r).query(t.Context(), "SELECT 1;\n")
			if err != nil || out != "1" {
				t.Fatalf("query = %q, %v", out, err)
			}
			if tc.r.psqlN != tc.psqlRuns {
				t.Fatalf("psql ran %d times, want %d", tc.r.psqlN, tc.psqlRuns)
			}
			if id := tc.r.psqlArgs[2]; id != tc.r.ps(tc.r.psN) {
				t.Fatalf("psql ran in %s, not the running container", id)
			}
			if args := tc.r.readyArgs[2:]; !slices.Equal(args, []string{"pg_isready", "-q", "-h", "127.0.0.1", "-U", "orglane", "-d", "postgres"}) {
				t.Fatalf("readiness probe = %v, want TCP", args)
			}
		})
	}
}

// TestPostgresFailsFast covers the errors that are not waited for, and the
// bound of the wait.
func TestPostgresFailsFast(t *testing.T) {
	for name, tc := range map[string]struct {
		r        *pgRunner
		psqlRuns int
		want     error
	}{
		"error in the SQL": {
			r:        &pgRunner{ps: always("c1"), ready: always(true), psql: always[error](exitError(3))},
			psqlRuns: 1,
		},
		"refused by the ready server": {
			// A role or database that does not exist: psql exits 2 too.
			r:        &pgRunner{ps: always("c1"), ready: always(true), psql: always[error](exitError(2))},
			psqlRuns: 2,
		},
		"psql fails in a running container": {
			r:        &pgRunner{ps: always("c1"), ready: always(true), psql: always[error](exitError(1))},
			psqlRuns: 1,
		},
		"never accepts connections": {
			r:    &pgRunner{ps: always("c1"), ready: always(false), psql: always[error](nil)},
			want: errPostgresNotReady,
		},
		"no container": {
			r:    &pgRunner{ps: always(""), ready: always(true), psql: always[error](nil)},
			want: errNoPostgres,
		},
		"keeps breaking": {
			r:        &pgRunner{ps: always("c1"), ready: func(n int) bool { return n%2 == 1 }, psql: always[error](exitError(2))},
			psqlRuns: -1, // as often as the deadline allows
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := testPostgres(tc.r)
			p.timeout = 200 * time.Millisecond
			start := time.Now()
			err := p.exec(t.Context(), "SELECT 1;\n", []string{"p4ss"})
			if err == nil {
				t.Fatal("exec succeeded")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Fatalf("exec = %v, want %v", err, tc.want)
			}
			if tc.psqlRuns >= 0 && tc.r.psqlN != tc.psqlRuns {
				t.Fatalf("psql ran %d times, want %d", tc.r.psqlN, tc.psqlRuns)
			}
			if tc.psqlRuns > 0 && time.Since(start) >= p.timeout {
				t.Fatalf("waited %v for a permanent error", time.Since(start))
			}
			if time.Since(start) > p.timeout+time.Second {
				t.Fatalf("waited %v, more than the bound %v", time.Since(start), p.timeout)
			}
			if strings.Contains(err.Error(), "p4ss") {
				t.Fatalf("the error carries a secret: %v", err)
			}
		})
	}
}

func TestPostgresStopsWithTheContext(t *testing.T) {
	r := &pgRunner{ps: always("c1"), ready: always(false), psql: always[error](nil)}
	p := testPostgres(r)
	p.timeout, p.wait = time.Hour, 10*time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := p.exec(ctx, "SELECT 1;\n", nil)
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, errPostgresNotReady) {
		t.Fatalf("exec = %v", err)
	}
}

func TestPostgresDockerErrorFailsFast(t *testing.T) {
	r := &recordingRunner{}
	p := postgres{r: r, project: "orglane", service: "postgres", user: "orglane", timeout: time.Hour}
	if err := p.exec(t.Context(), "SELECT 1;\n", nil); err == nil || errors.Is(err, errNoPostgres) {
		t.Fatalf("exec = %v", err)
	}
	if n := r.count(); n != 1 {
		t.Fatalf("%d Docker calls, want the one ps", n)
	}
}

// isPS reports a docker ps command.
func isPS(args []string) bool { return len(args) > 0 && args[0] == "ps" }

// isReadyProbe reports a pg_isready command.
func isReadyProbe(args []string) bool {
	return len(args) > 2 && args[0] == "exec" && args[2] == "pg_isready"
}

// nthCall makes match report true only for its nth matching command.
func nthCall(n int, match func([]string) bool) func([]string) bool {
	calls := 0
	return func(args []string) bool {
		if !match(args) {
			return false
		}
		calls++
		return calls == n
	}
}

// blockingRunner stalls the commands selected by block until their context
// ends, like a Docker daemon or container that stops answering.
type blockingRunner struct {
	*pgRunner
	block   func([]string) bool
	started chan struct{}
	once    sync.Once
}

// run implements runner.
func (r *blockingRunner) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	if r.block(args) {
		r.once.Do(func() { close(r.started) })
		<-ctx.Done()
		return nil, nil, context.Cause(ctx)
	}
	return r.pgRunner.run(ctx, stdin, args...)
}

// TestPostgresBoundsBlockedProbes covers a stalled docker ps or docker exec
// pg_isready: the probe must end with the readiness deadline, not wait for the
// caller, the agent's lifetime context. This includes both probes on the
// retry path after a psql connection failure.
func TestPostgresBoundsBlockedProbes(t *testing.T) {
	for name, tc := range map[string]struct {
		block func([]string) bool
		psql  func(int) error
	}{
		"container lookup":       {block: nthCall(1, isPS), psql: always[error](nil)},
		"readiness probe":        {block: nthCall(1, isReadyProbe), psql: always[error](nil)},
		"retry container lookup": {block: nthCall(2, isPS), psql: from[error](1, exitError(2), nil)},
		"retry readiness probe":  {block: nthCall(2, isReadyProbe), psql: from[error](1, exitError(2), nil)},
	} {
		t.Run(name, func(t *testing.T) {
			r := &blockingRunner{
				pgRunner: &pgRunner{ps: always("c1"), ready: always(true), psql: tc.psql},
				block:    tc.block,
				started:  make(chan struct{}),
			}
			p := testPostgres(r.pgRunner)
			p.r = r
			p.timeout = 100 * time.Millisecond
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- p.exec(ctx, "SELECT 1;\n", nil) }()
			select {
			case <-r.started:
			case <-time.After(2 * time.Second):
				t.Fatal("the probe did not run")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("exec accepted a stalled probe")
				}
			case <-time.After(2 * time.Second):
				cancel()
				t.Fatal("a stalled probe ignored the readiness deadline")
			}
		})
	}
}

// lateReadyRunner lets a pg_isready probe succeed, but only after its
// context ended.
type lateReadyRunner struct{ *pgRunner }

// run implements runner.
func (r *lateReadyRunner) run(ctx context.Context, stdin []byte, args ...string) ([]byte, []byte, error) {
	if isReadyProbe(args) {
		<-ctx.Done()
		return nil, nil, nil
	}
	return r.pgRunner.run(ctx, stdin, args...)
}

// TestPostgresRejectsLateReadiness makes sure a probe that returns success
// after the readiness deadline is not taken for a ready server.
func TestPostgresRejectsLateReadiness(t *testing.T) {
	r := &lateReadyRunner{&pgRunner{ps: always("c1"), ready: always(true), psql: always[error](nil)}}
	p := testPostgres(r.pgRunner)
	p.r = r
	p.timeout = 50 * time.Millisecond
	if err := p.exec(t.Context(), "SELECT 1;\n", nil); err == nil {
		t.Fatal("exec accepted a readiness probe that succeeded after the deadline")
	}
	if r.psqlN != 0 {
		t.Fatalf("psql ran %d times after a late readiness result, want none", r.psqlN)
	}
}

// transferRunner records the context of a dump or restore transfer.
type transferRunner struct {
	*pgRunner
	args        []string
	deadline    time.Time
	hasDeadline bool
	err         error
}

// pipe implements runner for dumps and restores.
func (r *transferRunner) pipe(ctx context.Context, _ io.Reader, _ io.Writer, args ...string) ([]byte, error) {
	r.args = args
	r.deadline, r.hasDeadline = ctx.Deadline()
	r.err = ctx.Err()
	return nil, nil
}

// TestPostgresTransfersKeepTheCallersLifetime makes sure waiting for
// readiness does not bound the single-shot dump and restore that follow it.
func TestPostgresTransfersKeepTheCallersLifetime(t *testing.T) {
	r := &transferRunner{pgRunner: &pgRunner{ps: always("c1"), ready: always(true)}}
	p := testPostgres(r.pgRunner)
	p.r = r
	p.timeout = 20 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	run := func(name, command string, transfer func() error) {
		t.Helper()
		r.args, r.deadline, r.hasDeadline, r.err = nil, time.Time{}, false, nil
		if err := transfer(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if r.err != nil {
			t.Fatalf("%s transfer context ended early: %v", name, r.err)
		}
		if !r.hasDeadline || time.Until(r.deadline) < 30*time.Second {
			t.Fatalf("%s transfer was bounded by the readiness deadline", name)
		}
		if !slices.Contains(r.args, command) {
			t.Fatalf("%s ran %v", name, r.args)
		}
	}
	run("dump", "pg_dump", func() error { return p.dump(ctx, "orglane_app", io.Discard) })
	run("restore", "pg_restore", func() error {
		return p.restore(ctx, "orglane_app", "orglane_app", strings.NewReader("dump"))
	})
}

// TestPostgresBlockedProbeStopsWithTheContext makes sure the readiness
// budget does not replace the caller's cancellation.
func TestPostgresBlockedProbeStopsWithTheContext(t *testing.T) {
	r := &blockingRunner{
		pgRunner: &pgRunner{ps: always("c1"), ready: always(true), psql: always[error](nil)},
		block:    nthCall(1, isReadyProbe),
		started:  make(chan struct{}),
	}
	p := testPostgres(r.pgRunner)
	p.r = r
	p.timeout = time.Hour
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.exec(ctx, "SELECT 1;\n", nil) }()
	<-r.started
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("exec = %v, want the caller's deadline", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		t.Fatal("a stalled probe did not stop with the caller's context")
	}
}
