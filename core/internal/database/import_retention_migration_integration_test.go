//go:build integration

package database_test

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/tests/pgtest"
)

func TestImportRetentionMigrationBackfillsAndRollsBack(t *testing.T) {
	ctx := t.Context()
	pool := connect(t, pgtest.NewDatabase(t))
	migrations := newGoose(t, pool)
	if _, err := migrations.UpTo(ctx, 12); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	person := uuid.New()
	exec("INSERT INTO people (id, username, work_email, display_name, status) VALUES ($1, 'uploader', 'uploader@acme.test', 'Uploader', 'active')", person)
	staged, audited, unaudited := uuid.New(), uuid.New(), uuid.New()
	insertRun := func(id uuid.UUID, state, age string) {
		t.Helper()
		exec(`INSERT INTO people_import_runs (id, created_by, source, encoding, delimiter, headers_sealed, state, created_at)
              VALUES ($1, $2, 'file', 'utf-8', ',', '\x01', $3, now() - $4::interval)`, id, person, state, age)
		exec("INSERT INTO people_import_rows (run_id, row_number, payload_sealed) VALUES ($1, 2, '\\x02')", id)
	}
	insertRun(staged, "previewed", "2 hours")
	insertRun(audited, "applied", "10 days")
	insertRun(unaudited, "applied", "20 days")
	exec("INSERT INTO audit_entries (occurred_at, actor_kind, actor_id, action, target_kind, target_id) VALUES (now() - interval '9 days', 'person', $1, 'import.applied', 'people_import', $2)", person.String(), audited.String())

	if _, err := migrations.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
	type lifecycle struct {
		created  time.Time
		applied  *time.Time
		expires  time.Time
		captions []string
	}
	read := func(id uuid.UUID) lifecycle {
		t.Helper()
		var l lifecycle
		if err := pool.QueryRow(ctx, "SELECT created_at, applied_at, expires_at, captions FROM people_import_runs WHERE id=$1", id).Scan(&l.created, &l.applied, &l.expires, &l.captions); err != nil {
			t.Fatal(err)
		}
		return l
	}
	if l := read(staged); l.applied != nil || !l.expires.Equal(l.created.Add(24*time.Hour)) || l.captions != nil {
		t.Fatalf("unfinished run backfill = %+v", l)
	}
	var appliedAudit time.Time
	if err := pool.QueryRow(ctx, "SELECT occurred_at FROM audit_entries WHERE target_id=$1", audited.String()).Scan(&appliedAudit); err != nil {
		t.Fatal(err)
	}
	if l := read(audited); l.applied == nil || !l.applied.Equal(appliedAudit) || !l.expires.Equal(appliedAudit.Add(90*24*time.Hour)) {
		t.Fatalf("applied run backfill = %+v, want the apply audit time %v", l, appliedAudit)
	}
	if l := read(unaudited); l.applied == nil || !l.applied.Equal(l.created) || !l.expires.Equal(l.created.Add(90*24*time.Hour)) {
		t.Fatalf("applied run without audit backfill = %+v", l)
	}
	// The new constraints hold the lifecycle together.
	for _, bad := range []string{
		"UPDATE people_import_runs SET applied_at = NULL WHERE id = $1",
		"UPDATE people_import_runs SET headers_sealed = NULL WHERE id = $1",
	} {
		if _, err := pool.Exec(ctx, bad, audited); err == nil {
			t.Fatalf("%s was accepted", bad)
		}
	}
	// Rolling back drops runs whose source material was erased: the earlier
	// schema cannot hold them. Every other run survives unchanged.
	exec("UPDATE people_import_runs SET headers_sealed = NULL, captions = ARRAY['username',''] WHERE id = $1", audited)
	exec("UPDATE people_import_rows SET payload_sealed = NULL WHERE run_id = $1", audited)
	if _, err := migrations.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var runs, rows int
	if err := pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM people_import_runs WHERE id <> $1), (SELECT count(*) FROM people_import_rows)", audited).Scan(&runs, &rows); err != nil || runs != 2 || rows != 2 {
		t.Fatalf("after rollback runs=%d rows=%d, %v", runs, rows, err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO people_import_rows (run_id, row_number) VALUES ($1, 3)", staged); err == nil {
		t.Fatal("rollback left payload_sealed nullable")
	}
	if _, err := migrations.UpByOne(ctx); err != nil {
		t.Fatal(err)
	}
}
