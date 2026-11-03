//go:build integration

// Package auditfault makes writing an audit entry fail on the database side,
// so integration tests can check that a mutation and its audit entries
// commit together or not at all (package audit).
//
// Inject creates a trigger on audit_entries in the test database that fails
// a chosen insert. AssertRollsBack runs a mutation with the fault, checks
// that it failed and left every table as it was, then runs it again without
// the fault and checks that it does change the database, so the first check
// is not vacuous.
package auditfault

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Code is the SQLSTATE of the injected failure.
const Code = "OLT01"

// Is reports whether err is (or wraps) the injected failure.
func Is(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == Code
}

// Expect checks the error a mutation returned in Mutation.Run: with the
// fault (faulty) it must be the injected failure; without, nil or one of
// allowed (for mutations that report an outcome as an error, such as a
// failed sign-in).
func Expect(faulty bool, err error, allowed ...error) error {
	switch {
	case faulty && err == nil:
		return errors.New("the mutation succeeded, want the injected audit failure")
	case faulty && !Is(err):
		return fmt.Errorf("the mutation failed with %w, want the injected audit failure (SQLSTATE %s)", err, Code)
	case !faulty && err != nil:
		for _, a := range allowed {
			if errors.Is(err, a) {
				return nil
			}
		}
		return fmt.Errorf("the mutation failed: %w", err)
	}
	return nil
}

// Fault is a trigger that fails an insert into audit_entries. It counts the
// inserts it failed in a sequence, which is not rolled back with the failed
// transaction.
type Fault struct {
	t    testing.TB
	pool *pgxpool.Pool
}

// Inject makes the nth audit entry insert of a transaction for which when is
// true fail; the earlier matching inserts of the transaction succeed. when is
// the trigger's WHEN condition, NEW being the entry inserted, e.g.
// "NEW.action = 'person.created'". The trigger is dropped when the test ends.
func Inject(t testing.TB, pool *pgxpool.Pool, when string, nth int) *Fault {
	t.Helper()
	f := &Fault{t: t, pool: pool}
	f.exec(`DROP TRIGGER IF EXISTS test_audit_fault ON audit_entries`)
	f.exec(`DROP SEQUENCE IF EXISTS test_audit_faults`)
	f.exec(`CREATE SEQUENCE test_audit_faults`)
	// A transaction-local setting counts the matching inserts of the
	// current transaction.
	f.exec(`CREATE OR REPLACE FUNCTION test_audit_fault() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    seen int := coalesce(nullif(current_setting('orglane_test.audit_fault_seen', true), ''), '0')::int + 1;
BEGIN
    PERFORM set_config('orglane_test.audit_fault_seen', seen::text, true);
    IF seen < TG_ARGV[0]::int THEN
        RETURN NULL;
    END IF;
    PERFORM nextval('test_audit_faults');
    RAISE EXCEPTION 'test: audit entry % rejected', NEW.action USING ERRCODE = '` + Code + `';
END;
$$`)
	f.exec(fmt.Sprintf(`CREATE TRIGGER test_audit_fault AFTER INSERT ON audit_entries
FOR EACH ROW WHEN (%s) EXECUTE FUNCTION test_audit_fault('%d')`, when, nth))
	t.Cleanup(f.Remove)
	return f
}

// Remove drops the trigger: audit entries are written normally again.
func (f *Fault) Remove() {
	f.exec(`DROP TRIGGER IF EXISTS test_audit_fault ON audit_entries`)
}

// Hits returns how many audit entry inserts the trigger failed.
func (f *Fault) Hits() int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM test_audit_faults`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *Fault) exec(sql string) {
	f.t.Helper()
	if _, err := f.pool.Exec(context.Background(), sql); err != nil {
		f.t.Fatalf("auditfault: %s: %v", sql, err)
	}
}

// State is the content of every table in the public schema, as JSON per
// table.
type State map[string]string

// Snapshot reads the content of every table in the public schema.
func Snapshot(t testing.TB, pool *pgxpool.Pool) State {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
	if err != nil {
		t.Fatal(err)
	}
	tables, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	state := make(State, len(tables))
	for _, table := range tables {
		var content string
		sql := `SELECT coalesce(jsonb_agg(to_jsonb(r) ORDER BY to_jsonb(r)::text), '[]')::text FROM ` + pgx.Identifier{table}.Sanitize() + ` r`
		if err := pool.QueryRow(ctx, sql).Scan(&content); err != nil {
			t.Fatalf("auditfault: read %s: %v", table, err)
		}
		state[table] = content
	}
	return state
}

// Changed returns the tables whose content differs in other, sorted.
func (s State) Changed(other State) []string {
	var changed []string
	for table, content := range other {
		if s[table] != content {
			changed = append(changed, table)
		}
	}
	for table := range s {
		if _, ok := other[table]; !ok {
			changed = append(changed, table)
		}
	}
	slices.Sort(changed)
	return changed
}

// Mutation is a change whose audit entry fails.
type Mutation struct {
	// Reject selects the audit entries that can fail (a WHEN condition, see
	// Inject).
	Reject string
	// Nth is which matching entry of the mutation's transaction fails
	// (default 1). Choosing a later one makes the earlier entries and
	// writes succeed first.
	Nth int
	// Tables is how many tables the mutation changes when it succeeds,
	// audit_entries included (default 2).
	Tables int
	// Run performs the mutation and checks its outcome: with the fault
	// (faulty) it must fail, without it must succeed. It returns an error
	// describing an unexpected outcome. It runs twice, so it must be
	// repeatable when nothing was committed.
	Run func(faulty bool) error
}

// AssertRollsBack runs m with its audit entry failing and checks that it
// failed because of the fault and left the database as it was. Then it runs
// m again without the fault and checks that it changed m.Tables tables,
// audit_entries among them, and wrote the entries that failed before.
func AssertRollsBack(t testing.TB, pool *pgxpool.Pool, m Mutation) {
	t.Helper()
	nth, tables := max(m.Nth, 1), m.Tables
	if tables == 0 {
		tables = 2
	}
	matching := `SELECT count(*) FROM audit_entries AS "new" WHERE ` + m.Reject
	countMatching := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(context.Background(), matching).Scan(&n); err != nil {
			t.Fatalf("auditfault: %s: %v", matching, err)
		}
		return n
	}

	fault := Inject(t, pool, m.Reject, nth)
	matchingBefore := countMatching()
	before := Snapshot(t, pool)
	if err := m.Run(true); err != nil {
		t.Fatalf("with the audit entry failing: %v", err)
	}
	if n := fault.Hits(); n != 1 {
		t.Fatalf("the fault failed %d audit entry inserts, want 1", n)
	}
	if changed := before.Changed(Snapshot(t, pool)); len(changed) != 0 {
		t.Fatalf("the failed mutation changed %v; it must roll back completely", changed)
	}

	fault.Remove()
	if err := m.Run(false); err != nil {
		t.Fatalf("without the fault: %v", err)
	}
	changed := before.Changed(Snapshot(t, pool))
	if len(changed) != tables || !slices.Contains(changed, "audit_entries") {
		t.Fatalf("without the fault the mutation changed %v, want %d tables including audit_entries", changed, tables)
	}
	if n := countMatching() - matchingBefore; n != nth {
		t.Fatalf("without the fault the mutation wrote %d entries matching %q, want %d", n, m.Reject, nth)
	}
}
