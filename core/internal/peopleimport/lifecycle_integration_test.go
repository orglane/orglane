//go:build integration

package peopleimport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/peopleimport"
)

// sealedLeft counts the sealed bytes still stored for a run.
func (f *fixture) sealedLeft(id uuid.UUID) int {
	f.t.Helper()
	return f.count(`SELECT (SELECT count(*) FROM people_import_runs WHERE id=$1 AND headers_sealed IS NOT NULL)
        + (SELECT count(*) FROM people_import_rows WHERE run_id=$1 AND payload_sealed IS NOT NULL)
        + (SELECT count(*) FROM people_import_rows WHERE run_id=$1 AND preview_sealed IS NOT NULL)`, id)
}

func (f *fixture) runExists(id uuid.UUID) bool {
	f.t.Helper()
	return f.count("SELECT count(*) FROM people_import_runs WHERE id=$1", id) == 1
}

func (f *fixture) auditCount(action string, id uuid.UUID) int {
	f.t.Helper()
	return f.count("SELECT count(*) FROM audit_entries WHERE action=$1 AND target_kind='people_import' AND target_id=$2", action, id.String())
}

// person signs in another person with the given built-in role.
func (f *fixture) person(username string, role authz.RoleKey) (context.Context, audit.Actor) {
	f.t.Helper()
	p, err := f.ids.CreatePerson(f.t.Context(), audit.System, identity.NewPerson{Username: username, WorkEmail: username + "@acme.test", DisplayName: username, Status: identity.StatusActive})
	if err != nil {
		f.t.Fatal(err)
	}
	if err = f.ids.AssignRole(f.t.Context(), audit.System, p.ID, role); err != nil {
		f.t.Fatal(err)
	}
	ctx := authz.WithPrincipal(f.t.Context(), &authz.Principal{Subject: p.ID.String(), Permissions: authz.All()})
	return ctx, audit.Actor{Kind: audit.ActorPerson, ID: p.ID.String()}
}

func notFound(t *testing.T, f *fixture, id uuid.UUID) {
	t.Helper()
	if _, err := f.imports.Get(f.ctx, f.actor, id); !errors.Is(err, peopleimport.ErrNotFound) {
		t.Fatalf("get = %v, want not found", err)
	}
	if _, err := f.imports.Preview(f.ctx, f.actor, id, mapping("username", "work_email", "display_name")); !errors.Is(err, peopleimport.ErrNotFound) {
		t.Fatalf("preview = %v, want not found", err)
	}
	if _, err := f.imports.Apply(f.ctx, f.actor, id); !errors.Is(err, peopleimport.ErrNotFound) {
		t.Fatalf("apply = %v, want not found", err)
	}
}

func TestApplyErasesSourceMaterialAndKeepsValueFreeResult(t *testing.T) {
	f := newFixture(t)
	run := f.uploadText("username,work_email,display_name,Secret Heading\nerased,erased@acme.test,Erased Person,private-cell\nshort,short@acme.test\n")
	if want := run.CreatedAt.Add(peopleimport.StagingRetention); !run.ExpiresAt.Equal(want) {
		t.Fatalf("staged expiresAt = %v, want upload + 24h = %v", run.ExpiresAt, want)
	}
	run = f.preview(run, mapping("username", "work_email", "display_name"))
	assertDecision(t, run, "create", "conflict")
	if n := f.sealedLeft(run.ID); n != 5 {
		t.Fatalf("previewed run has %d sealed values, want headers, 2 payloads and 2 previews", n)
	}
	headers := run.Headers
	if !slices.Equal(headers, []string{"username", "work_email", "display_name", ""}) {
		t.Fatalf("captions = %q", headers)
	}
	before := time.Now()
	applied := f.apply(run)
	if f.sealedLeft(run.ID) != 0 {
		t.Fatal("sealed source material remains after apply")
	}
	var stored []byte
	if err := f.pool.QueryRow(f.ctx, `SELECT jsonb_build_object(
        'run', (SELECT to_jsonb(r) FROM people_import_runs r WHERE id=$1),
        'rows', (SELECT jsonb_agg(to_jsonb(w)) FROM people_import_rows w WHERE run_id=$1))::text`, run.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"erased@", "Erased Person", "Secret", "private-cell", "short@"} {
		if bytes.Contains(stored, []byte(value)) {
			t.Fatalf("stored run still holds %q", value)
		}
	}
	var appliedAt time.Time
	if err := f.pool.QueryRow(f.ctx, "SELECT applied_at FROM people_import_runs WHERE id=$1", run.ID).Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	if appliedAt.Before(before.Add(-time.Minute)) || !applied.ExpiresAt.Equal(appliedAt.Add(peopleimport.AppliedRetention)) {
		t.Fatalf("applied at %v expires %v, want apply + 90 days", appliedAt, applied.ExpiresAt)
	}
	if applied.Rows[0].Result != "created" || applied.Rows[1].Result != "failed" || applied.Rows[1].Error != "column_count" {
		t.Fatalf("results = %+v", applied.Rows)
	}
	if !slices.Equal(applied.Headers, headers) {
		t.Fatalf("captions after erase = %q, want %q", applied.Headers, headers)
	}
	// Every view of the applied run works from value-free data only.
	got, err := f.imports.Get(f.ctx, f.actor, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	auditBefore := f.count("SELECT count(*) FROM audit_entries")
	again := f.apply(run)
	if f.count("SELECT count(*) FROM audit_entries") != auditBefore {
		t.Fatal("repeated apply wrote audit entries")
	}
	a, _ := json.Marshal(applied)
	b, _ := json.Marshal(got)
	c, _ := json.Marshal(again)
	if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
		t.Fatalf("applied, read and repeated results differ:\n%s\n%s\n%s", a, b, c)
	}
	if csv := peopleimport.ErrorsCSV(got); csv != "row,decision,error,result\n3,conflict,column_count,failed\n" {
		t.Fatalf("error export after erase = %q", csv)
	}
	if _, err = f.imports.Preview(f.ctx, f.actor, run.ID, mapping("username", "work_email", "display_name")); !errors.Is(err, peopleimport.ErrState) {
		t.Fatalf("preview of applied run = %v, want state error", err)
	}
	if f.count("SELECT count(*) FROM people WHERE username='erased'") != 1 {
		t.Fatal("apply did not create the person")
	}
}

func TestCleanupExpiresUnfinishedAndAppliedRuns(t *testing.T) {
	f := newFixture(t)
	text := func(u string) string {
		return "username,work_email,display_name,external_id\n" + u + "," + u + "@acme.test," + u + ",ext-" + u + "\n"
	}
	m := mapping("username", "work_email", "display_name", "external_id")
	staged := f.uploadText(text("staged"))
	previewed := f.preview(f.uploadText(text("previewed")), m)
	applied := f.apply(f.preview(f.uploadText(text("applied")), m))
	fresh := f.uploadText(text("fresh"))
	recent := f.apply(f.preview(f.uploadText(text("recent")), m))
	identities := f.count("SELECT count(*) FROM people_import_identities")
	if identities != 2 {
		t.Fatalf("identities = %d, want 2", identities)
	}
	// Age the runs: unfinished ones 25 hours after upload, the applied one
	// 91 days after apply. Expiry was fixed when the run was created/applied.
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := f.pool.Exec(f.ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []uuid.UUID{staged.ID, previewed.ID} {
		exec("UPDATE people_import_runs SET created_at = created_at - interval '25 hours', expires_at = expires_at - interval '25 hours' WHERE id=$1", id)
	}
	exec("UPDATE people_import_runs SET created_at = created_at - interval '91 days', applied_at = applied_at - interval '91 days', expires_at = expires_at - interval '91 days' WHERE id=$1", applied.ID)
	// Fresh runs stay just inside their limits.
	exec("UPDATE people_import_runs SET created_at = created_at - interval '23 hours', expires_at = expires_at - interval '23 hours' WHERE id=$1", fresh.ID)
	exec("UPDATE people_import_runs SET applied_at = applied_at - interval '89 days', expires_at = expires_at - interval '89 days' WHERE id=$1", recent.ID)

	// Expired runs are gone before the job deletes them.
	for _, id := range []uuid.UUID{staged.ID, previewed.ID, applied.ID} {
		notFound(t, f, id)
	}
	res, err := f.imports.Cleanup(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if res.Expired != 3 || res.Erased != 0 {
		t.Fatalf("cleanup = %+v, want 3 expired", res)
	}
	for _, id := range []uuid.UUID{staged.ID, previewed.ID, applied.ID} {
		if f.runExists(id) || f.count("SELECT count(*) FROM people_import_rows WHERE run_id=$1", id) != 0 {
			t.Fatalf("expired run %s remains", id)
		}
		if f.count("SELECT count(*) FROM audit_entries WHERE action='import.expired' AND actor_kind='system' AND actor_id IS NULL AND target_id=$1", id.String()) != 1 {
			t.Fatalf("expiry of %s not audited by the system actor", id)
		}
		notFound(t, f, id)
	}
	if _, err = f.imports.Get(f.ctx, f.actor, fresh.ID); err != nil {
		t.Fatalf("fresh staged run: %v", err)
	}
	if r, err := f.imports.Get(f.ctx, f.actor, recent.ID); err != nil || r.Rows[0].Result != "created" {
		t.Fatalf("recent applied run: %v", err)
	}
	if f.count("SELECT count(*) FROM people_import_identities") != identities || f.count("SELECT count(*) FROM people WHERE username='applied'") != 1 {
		t.Fatal("expiry touched people or source identity matches")
	}
	// Idempotent: nothing is left to do and nothing more is audited.
	auditBefore := f.count("SELECT count(*) FROM audit_entries")
	if res, err = f.imports.Cleanup(f.ctx); err != nil || res != (peopleimport.CleanupResult{}) {
		t.Fatalf("second cleanup = %+v, %v", res, err)
	}
	if f.count("SELECT count(*) FROM audit_entries") != auditBefore {
		t.Fatal("idle cleanup wrote audit entries")
	}
	var metadata []byte
	if err = f.pool.QueryRow(f.ctx, "SELECT metadata::text FROM audit_entries WHERE action='import.expired' AND target_id=$1", applied.ID.String()).Scan(&metadata); err != nil || string(metadata) != `{"state": "applied"}` {
		t.Fatalf("expiry metadata = %s, %v", metadata, err)
	}
}

func TestCleanupErasesSourceMaterialOfEarlierApplies(t *testing.T) {
	f := newFixture(t)
	run := f.preview(f.uploadText("username,work_email,display_name,Private Heading\nlegacy,legacy@acme.test,Legacy,private\n"), mapping("username", "work_email", "display_name"))
	// A run applied by a version before apply erased its source material.
	if _, err := f.pool.Exec(f.ctx, "UPDATE people_import_runs SET state='applied', applied_at=now(), expires_at=now() + interval '90 days' WHERE id=$1", run.ID); err != nil {
		t.Fatal(err)
	}
	res, err := f.imports.Cleanup(f.ctx)
	if err != nil || res.Erased != 1 || res.Expired != 0 {
		t.Fatalf("cleanup = %+v, %v", res, err)
	}
	if f.sealedLeft(run.ID) != 0 || f.auditCount("import.source_erased", run.ID) != 1 {
		t.Fatal("legacy source material not erased and audited")
	}
	got, err := f.imports.Get(f.ctx, f.actor, run.ID)
	if err != nil || !slices.Equal(got.Headers, run.Headers) {
		t.Fatalf("captions after erase = %q, %v; want %q", got.Headers, err, run.Headers)
	}
	if res, err = f.imports.Cleanup(f.ctx); err != nil || res != (peopleimport.CleanupResult{}) {
		t.Fatalf("second cleanup = %+v, %v", res, err)
	}
}

func TestDeleteRunPermissionsAuditAndIdempotence(t *testing.T) {
	f := newFixture(t)
	adminCtx, adminActor := f.person("admin", authz.RoleAdmin)
	memberCtx, memberActor := f.person("member", authz.RoleMember)
	m := mapping("username", "work_email", "display_name", "external_id")
	own := f.uploadText("username,work_email,display_name,external_id\nown,own@acme.test,Own,ext-own\n")
	other := f.apply(f.preview(f.uploadText("username,work_email,display_name,external_id\nother,other@acme.test,Other Person,ext-other\n"), m))

	// Without people.manage nobody may delete, and nothing is recorded.
	if err := f.imports.Delete(memberCtx, memberActor, own.ID); !errors.Is(err, peopleimport.ErrForbidden) {
		t.Fatalf("member delete = %v, want forbidden", err)
	}
	reduced := authz.WithPrincipal(f.t.Context(), &authz.Principal{Subject: f.owner.ID.String(), Permissions: []authz.Permission{authz.InstanceRead}})
	if err := f.imports.Delete(reduced, f.actor, own.ID); !errors.Is(err, peopleimport.ErrForbidden) {
		t.Fatalf("delegated delete without people.manage = %v, want forbidden", err)
	}
	if !f.runExists(own.ID) || f.auditCount("import.deleted", own.ID) != 0 {
		t.Fatal("denied delete changed state")
	}

	// The uploader deletes an unfinished run.
	if err := f.imports.Delete(f.ctx, f.actor, own.ID); err != nil {
		t.Fatal(err)
	}
	if f.runExists(own.ID) || f.count("SELECT count(*) FROM people_import_rows WHERE run_id=$1", own.ID) != 0 {
		t.Fatal("deleted run remains")
	}
	notFound(t, f, own.ID)

	// Another people.manage holder deletes the owner's applied run.
	identities := f.count("SELECT count(*) FROM people_import_identities")
	if err := f.imports.Delete(adminCtx, adminActor, other.ID); err != nil {
		t.Fatal(err)
	}
	if f.runExists(other.ID) || f.count("SELECT count(*) FROM people WHERE username='other'") != 1 || f.count("SELECT count(*) FROM people_import_identities") != identities {
		t.Fatal("delete removed the run's people or identity matches, or kept the run")
	}
	notFound(t, f, other.ID)

	// Audited once, by the deleting person, with the run id and no values.
	var actorID, metadata string
	if err := f.pool.QueryRow(f.ctx, "SELECT actor_id, metadata::text FROM audit_entries WHERE action='import.deleted' AND target_id=$1", other.ID.String()).Scan(&actorID, &metadata); err != nil {
		t.Fatal(err)
	}
	if actorID != adminActor.ID || metadata != `{"state": "applied"}` {
		t.Fatalf("delete audit actor=%s metadata=%s", actorID, metadata)
	}
	// Idempotent: deleting again, or an unknown id, succeeds without audit.
	auditBefore := f.count("SELECT count(*) FROM audit_entries")
	for _, id := range []uuid.UUID{own.ID, other.ID, uuid.New()} {
		if err := f.imports.Delete(f.ctx, f.actor, id); err != nil {
			t.Fatalf("repeated delete = %v", err)
		}
	}
	if f.count("SELECT count(*) FROM audit_entries") != auditBefore || f.auditCount("import.deleted", own.ID) != 1 {
		t.Fatal("repeated delete wrote audit entries")
	}
}

// waitLocks waits until n other sessions of the test database wait for a lock.
func (f *fixture) waitLocks(n int) {
	f.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if f.count("SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'") >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("fewer than %d sessions block on the import locks", n)
}

// holdRun locks a run row the way a concurrent preview or apply does, so the
// test controls in which order the competing operations reach it.
func (f *fixture) holdRun(id uuid.UUID, sql string) pgx.Tx {
	f.t.Helper()
	tx, err := f.pool.Begin(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	if _, err = tx.Exec(f.ctx, "SELECT 1 FROM people_import_runs WHERE id=$1 FOR UPDATE", id); err != nil {
		f.t.Fatal(err)
	}
	if sql != "" {
		if _, err = tx.Exec(f.ctx, sql, id); err != nil {
			f.t.Fatal(err)
		}
	}
	return tx
}

type applyResult struct {
	run peopleimport.Run
	err error
}

func (f *fixture) applyAsync(id uuid.UUID) <-chan applyResult {
	out := make(chan applyResult, 1)
	go func() {
		r, err := f.imports.Apply(f.ctx, f.actor, id)
		out <- applyResult{r, err}
	}()
	return out
}
func async(fn func() error) <-chan error {
	out := make(chan error, 1)
	go func() { out <- fn() }()
	return out
}

func TestDeleteAndCleanupRacingApply(t *testing.T) {
	text := "username,work_email,display_name\nrace,race@acme.test,Race\nrace2,race2@acme.test,Race Two\n"
	m := mapping("username", "work_email", "display_name")
	appliedFully := func(t *testing.T, f *fixture, r applyResult) {
		t.Helper()
		if r.err != nil || r.run.State != "applied" || r.run.Rows[0].Result != "created" || r.run.Rows[1].Result != "created" {
			t.Fatalf("apply = %+v, %v", r.run, r.err)
		}
		if f.count("SELECT count(*) FROM people WHERE username IN ('race','race2')") != 2 {
			t.Fatal("apply was cut short")
		}
	}
	untouched := func(t *testing.T, f *fixture, r applyResult, id uuid.UUID) {
		t.Helper()
		if !errors.Is(r.err, peopleimport.ErrNotFound) {
			t.Fatalf("apply after removal = %v, want not found", r.err)
		}
		if f.runExists(id) || f.count("SELECT count(*) FROM people WHERE username IN ('race','race2')") != 0 || f.auditCount("import.applied", id) != 0 {
			t.Fatal("apply of a removed run changed data")
		}
	}

	t.Run("delete waits for a running apply", func(t *testing.T) {
		f := newFixture(t)
		run := f.preview(f.uploadText(text), m)
		hold := f.holdRun(run.ID, "")
		applied := f.applyAsync(run.ID)
		f.waitLocks(1)
		deleted := async(func() error { return f.imports.Delete(f.ctx, f.actor, run.ID) })
		f.waitLocks(2)
		if err := hold.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		appliedFully(t, f, <-applied)
		if err := <-deleted; err != nil || f.runExists(run.ID) || f.auditCount("import.deleted", run.ID) != 1 {
			t.Fatalf("delete after apply = %v", err)
		}
	})
	t.Run("apply after a delete finds nothing", func(t *testing.T) {
		f := newFixture(t)
		run := f.preview(f.uploadText(text), m)
		hold := f.holdRun(run.ID, "")
		deleted := async(func() error { return f.imports.Delete(f.ctx, f.actor, run.ID) })
		f.waitLocks(1)
		applied := f.applyAsync(run.ID)
		f.waitLocks(2)
		if err := hold.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-deleted; err != nil {
			t.Fatal(err)
		}
		untouched(t, f, <-applied, run.ID)
	})
	t.Run("cleanup never deletes a run that apply took before it expired", func(t *testing.T) {
		f := newFixture(t)
		run := f.preview(f.uploadText(text), m)
		// The run expires while apply waits for it.
		hold := f.holdRun(run.ID, "UPDATE people_import_runs SET expires_at = now() + interval '1 second' WHERE id=$1")
		applied := f.applyAsync(run.ID)
		f.waitLocks(1)
		// Cleanup starts once the run has expired, while apply still holds it.
		time.Sleep(1200 * time.Millisecond)
		cleaned := async(func() error { _, err := f.imports.Cleanup(f.ctx); return err })
		f.waitLocks(2)
		if err := hold.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		r := <-applied
		appliedFully(t, f, r)
		if err := <-cleaned; err != nil {
			t.Fatal(err)
		}
		if !f.runExists(run.ID) || f.sealedLeft(run.ID) != 0 || f.auditCount("import.expired", run.ID) != 0 {
			t.Fatal("cleanup deleted or half-deleted an applied run")
		}
		if got, err := f.imports.Get(f.ctx, f.actor, run.ID); err != nil || len(got.Rows) != 2 {
			t.Fatalf("applied run after cleanup: %+v, %v", got, err)
		}
	})
	t.Run("apply after cleanup finds nothing", func(t *testing.T) {
		f := newFixture(t)
		run := f.preview(f.uploadText(text), m)
		if _, err := f.pool.Exec(f.ctx, "UPDATE people_import_runs SET expires_at = now() - interval '1 second' WHERE id=$1", run.ID); err != nil {
			t.Fatal(err)
		}
		hold := f.holdRun(run.ID, "")
		cleaned := async(func() error { _, err := f.imports.Cleanup(f.ctx); return err })
		f.waitLocks(1)
		applied := f.applyAsync(run.ID)
		f.waitLocks(2)
		if err := hold.Commit(f.ctx); err != nil {
			t.Fatal(err)
		}
		if err := <-cleaned; err != nil {
			t.Fatal(err)
		}
		untouched(t, f, <-applied, run.ID)
		if f.auditCount("import.expired", run.ID) != 1 {
			t.Fatal("expiry not audited")
		}
	})
}
