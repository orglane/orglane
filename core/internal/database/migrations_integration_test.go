//go:build integration

package database_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/orglane/orglane/core/migrations"
	"github.com/orglane/orglane/tests/pgtest"
)

// coreTables are the tables the core migrations create.
var coreTables = []string{
	"instance_settings",
	"people", "roles", "role_permissions", "person_roles", "devices", "sessions",
	"audit_entries",
	"person_credentials", "passkeys", "webauthn_ceremonies", "sign_in_throttles",
	"oidc_clients", "oidc_auth_requests", "oidc_refresh_tokens", "oidc_access_tokens", "signing_keys",
	"person_avatars",
}

func newGoose(t *testing.T, pool *pgxpool.Pool) *goose.Provider {
	t.Helper()
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	p, err := goose.NewProvider(goose.DialectPostgres, db, migrations.FS, goose.WithDisableGlobalRegistry(true))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// schemaObjects lists the tables and functions in the public schema, except
// goose's version table.
func schemaObjects(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT 'table ' || tablename FROM pg_tables WHERE schemaname = 'public' AND tablename <> 'goose_db_version'
		UNION ALL
		SELECT 'function ' || p.proname FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'public'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	var objects []string
	for rows.Next() {
		var o string
		if err := rows.Scan(&o); err != nil {
			t.Fatal(err)
		}
		objects = append(objects, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return objects
}

// TestCoreMigrationsUpDown applies every migration, rolls it back and applies
// it again, one at a time, then rolls everything back to an empty schema and
// applies it all once more.
func TestCoreMigrationsUpDown(t *testing.T) {
	ctx := context.Background()
	pool := connect(t, pgtest.NewDatabase(t))
	p := newGoose(t, pool)

	sources := p.ListSources()
	if len(sources) < 3 {
		t.Fatalf("found %d migrations, want at least 3", len(sources))
	}
	for _, src := range sources {
		before := schemaObjects(t, pool)
		if _, err := p.UpByOne(ctx); err != nil {
			t.Fatalf("up %s: %v", src.Path, err)
		}
		after := schemaObjects(t, pool)
		if _, err := p.Down(ctx); err != nil {
			t.Fatalf("down %s: %v", src.Path, err)
		}
		if got := schemaObjects(t, pool); !slices.Equal(got, before) {
			t.Fatalf("down %s left %v, want %v", src.Path, got, before)
		}
		if _, err := p.UpByOne(ctx); err != nil {
			t.Fatalf("re-apply %s: %v", src.Path, err)
		}
		if got := schemaObjects(t, pool); !slices.Equal(got, after) {
			t.Fatalf("re-applying %s gave %v, want %v", src.Path, got, after)
		}
	}
	for _, table := range coreTables {
		var exists bool
		if err := pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
			t.Errorf("table %s exists = %v, %v", table, exists, err)
		}
	}

	if _, err := p.DownTo(ctx, 0); err != nil {
		t.Fatalf("down to 0: %v", err)
	}
	if got := schemaObjects(t, pool); len(got) != 0 {
		t.Fatalf("after rolling back every migration the schema still has %v", got)
	}
	if _, err := p.Up(ctx); err != nil {
		t.Fatalf("up after full rollback: %v", err)
	}
}

func TestConnectionsUseUTC(t *testing.T) {
	ctx := context.Background()
	pool := connect(t, pgtest.NewDatabase(t))
	var tz string
	var now time.Time
	if err := pool.QueryRow(ctx, "SELECT current_setting('TimeZone'), now()").Scan(&tz, &now); err != nil {
		t.Fatal(err)
	}
	if tz != "UTC" {
		t.Errorf("session TimeZone = %q, want UTC", tz)
	}
	if now.Location() != time.UTC {
		t.Errorf("timestamptz scanned in %v, want UTC", now.Location())
	}
}
