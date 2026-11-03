//go:build integration

// Package pgtest runs a real PostgreSQL in a container for integration tests
// (testcontainers-go). Tests using it carry the "integration" build tag and
// need Docker.
//
// Usage: call Main (or Run) from TestMain, then NewDatabase in each test for a fresh,
// empty database.
package pgtest

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Image is the PostgreSQL image used by integration tests.
const Image = "postgres:18-alpine"

var (
	adminURL string
	counter  atomic.Int64
)

// Main runs the tests with Run and exits with its code.
func Main(m *testing.M) {
	os.Exit(Run(m))
}

// Run starts the container, runs the tests, removes the container and
// returns the exit code.
func Run(m *testing.M) int {
	ctx := context.Background()
	ctr, err := start(ctx)
	defer func() {
		if err := testcontainers.TerminateContainer(ctr); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: terminate container: %v\n", err)
		}
	}()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: start %s (is Docker running?): %v\n", Image, err)
		return 1
	}
	adminURL, err = ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		fmt.Fprintf(os.Stderr, "pgtest: connection string: %v\n", err)
		return 1
	}
	return m.Run()
}

// start runs the container. It retries because test packages start in
// parallel processes that race for the shared testcontainers reaper.
func start(ctx context.Context) (*postgres.PostgresContainer, error) {
	var err error
	for attempt := 1; attempt <= 3; attempt++ {
		var ctr *postgres.PostgresContainer
		ctr, err = postgres.Run(ctx, Image,
			postgres.WithDatabase("orglane"),
			postgres.WithUsername("orglane"),
			postgres.WithPassword("orglane"),
			testcontainers.WithWaitStrategy(
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2).WithStartupTimeout(60*time.Second),
				wait.ForListeningPort("5432/tcp"),
			),
		)
		if err == nil {
			return ctr, nil
		}
		_ = testcontainers.TerminateContainer(ctr)
		fmt.Fprintf(os.Stderr, "pgtest: attempt %d to start %s failed: %v\n", attempt, Image, err)
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return nil, err
}

// NewDatabase creates an empty database and returns its connection URL. The
// database is dropped when the test ends.
func NewDatabase(t testing.TB) string {
	t.Helper()
	if adminURL == "" {
		t.Fatal("pgtest: call pgtest.Main from TestMain")
	}
	ctx := context.Background()
	name := fmt.Sprintf("test_%d_%d", os.Getpid(), counter.Add(1))

	exec := func(sql string) {
		t.Helper()
		conn, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Fatalf("pgtest: connect: %v", err)
		}
		defer func() { _ = conn.Close(ctx) }()
		if _, err := conn.Exec(ctx, sql); err != nil {
			t.Fatalf("pgtest: %s: %v", sql, err)
		}
	}
	exec("CREATE DATABASE " + name)
	t.Cleanup(func() { exec("DROP DATABASE " + name + " WITH (FORCE)") })

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	return u.String()
}
