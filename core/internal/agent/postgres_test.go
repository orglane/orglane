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

func (e exitError) Error() string { return "exit status " + strconv.Itoa(int(e)) }
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

func (r *pgRunner) tail(context.Context, int, ...string) ([]byte, bool, []byte, error) {
	return nil, false, nil, errors.New("unexpected tail")
}

func (r *pgRunner) pipe(_ context.Context, _ io.Reader, _ io.Writer, args ...string) ([]byte, error) {
	return nil, errors.New("unexpected pipe " + strings.Join(args, " "))
}

func (r *pgRunner) stream(context.Context, ...string) (io.ReadCloser, func() error, error) {
	return nil, nil, errors.New("unexpected stream")
}

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
