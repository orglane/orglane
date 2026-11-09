//go:build integration

// Package pgtest runs a real PostgreSQL in a container for integration tests
// (testcontainers-go). Tests using it carry the "integration" build tag and
// need Docker.
//
// Usage: call Main (or Run, or RunWithTemplate) from TestMain, then
// NewDatabase in each test for a fresh, empty database.
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
	// template is the name of the prepared template database, or "".
	template string
	counter  atomic.Int64
)

// Main runs the tests with Run and exits with its code.
func Main(m *testing.M) {
	os.Exit(Run(m))
}

// Run starts the container, runs the tests, removes the container and
// returns the exit code.
func Run(m *testing.M) int {
	return run(m, nil)
}

// RunWithTemplate is Run with a template database: prepare runs once against
// a fresh, empty database before the tests, and NewDatabase creates every
// test's database from it with CREATE DATABASE ... TEMPLATE. Applying the
// migrations once per process instead of once per test is much faster; the
// databases stay independent, so tests keep their isolation.
//
// prepare must close every connection it opened: PostgreSQL refuses to copy a
// template database that another session is connected to.
func RunWithTemplate(m *testing.M, prepare func(ctx context.Context, databaseURL string) error) int {
	if prepare == nil {
		panic("pgtest: RunWithTemplate needs a prepare function")
	}
	return run(m, prepare)
}

func run(m *testing.M, prepare func(ctx context.Context, databaseURL string) error) int {
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
	if prepare != nil {
		const name = "orglane_template"
		if err := execSQL(ctx, "CREATE DATABASE "+name); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: create the template database: %v\n", err)
			return 1
		}
		if err := prepare(ctx, databaseURL(name)); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: prepare the template database: %v\n", err)
			return 1
		}
		// prepare closed its pool, but make sure nothing lingers: CREATE
		// DATABASE ... TEMPLATE fails while any session uses the template.
		if err := execSQL(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '"+name+"' AND pid <> pg_backend_pid()"); err != nil {
			fmt.Fprintf(os.Stderr, "pgtest: close the template's connections: %v\n", err)
			return 1
		}
		template = name
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
		t.Fatal("pgtest: call pgtest.Main, pgtest.Run or pgtest.RunWithTemplate from TestMain")
	}
	ctx := context.Background()
	name := fmt.Sprintf("test_%d_%d", os.Getpid(), counter.Add(1))
	sql := "CREATE DATABASE " + name
	if template != "" {
		sql += " TEMPLATE " + template
	}
	if err := execSQL(ctx, sql); err != nil {
		t.Fatalf("pgtest: %v", err)
	}
	t.Cleanup(func() {
		if err := execSQL(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("pgtest: dropping %s: %v", name, err)
		}
	})
	return databaseURL(name)
}

// databaseURL returns the connection URL of a database in the container.
func databaseURL(name string) string {
	u, err := url.Parse(adminURL)
	if err != nil {
		panic("pgtest: " + err.Error())
	}
	u.Path = "/" + name
	return u.String()
}

func execSQL(ctx context.Context, sql string) error {
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	if _, err := conn.Exec(ctx, sql); err != nil {
		return fmt.Errorf("%s: %w", sql, err)
	}
	return nil
}
