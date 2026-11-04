//go:build integration

package peopleimport_test

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/peopleimport"
	"github.com/orglane/orglane/core/internal/profile"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/tests/auditfault"
	"github.com/orglane/orglane/tests/pgtest"
)

//go:embed testdata
var fixtureFiles embed.FS

func TestMain(m *testing.M) { pgtest.Main(m) }

type fixture struct {
	t        *testing.T
	ctx      context.Context
	actor    audit.Actor
	pool     *pgxpool.Pool
	ids      *identity.Service
	profiles *profile.Service
	imports  *peopleimport.Service
	owner    store.Person
	keys     *crypto.Keyring
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := t.Context()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := database.Connect(ctx, config.Database{URL: pgtest.NewDatabase(t), MaxConns: 8, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	migrations, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err = migrations.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = migrations.Close()
	ids := identity.NewService(pool)
	if err = ids.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	owner, err := ids.CreatePerson(ctx, audit.System, identity.NewPerson{Username: "owner", WorkEmail: "owner@acme.test", DisplayName: "Owner", Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err = ids.AssignRole(ctx, audit.System, owner.ID, authz.RoleOwner); err != nil {
		t.Fatal(err)
	}
	keys, err := crypto.NewKeyring("import-test-master-secret-at-least-32-characters")
	if err != nil {
		t.Fatal(err)
	}
	profiles := profile.NewService(pool, keys, profile.WithTeams(ids))
	if _, err = profiles.CreateField(ctx, audit.System, profile.NewField{Key: "employee_code", Label: "Employee code", Type: profile.TypeText, Sensitive: true}); err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Kind: audit.ActorPerson, ID: owner.ID.String()}
	ctx = authz.WithPrincipal(ctx, &authz.Principal{Subject: owner.ID.String(), Permissions: authz.All()})
	return &fixture{t: t, ctx: ctx, actor: actor, pool: pool, ids: ids, profiles: profiles, imports: peopleimport.NewService(pool, ids, profiles, keys), owner: owner, keys: keys}
}
func (f *fixture) upload(name string) peopleimport.Run {
	f.t.Helper()
	file, err := fixtureFiles.Open("testdata/" + name)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	r, err := f.imports.Upload(f.ctx, f.actor, peopleimport.FileSource{}, file)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func (f *fixture) uploadText(text string) peopleimport.Run {
	f.t.Helper()
	r, err := f.imports.Upload(f.ctx, f.actor, peopleimport.FileSource{}, strings.NewReader(text))
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func mapping(targets ...string) peopleimport.Mapping {
	m := peopleimport.Mapping{Columns: []peopleimport.Column{}, CreateTeams: true, SkipRows: []int{}}
	for i, target := range targets {
		if target != "" {
			m.Columns = append(m.Columns, peopleimport.Column{Column: i, Target: target})
		}
	}
	return m
}
func goodMapping() peopleimport.Mapping {
	return mapping("username", "work_email", "contact_email", "display_name", "title", "teams", "role", "manager_email", "external_id", "profile.employee_code")
}
func (f *fixture) preview(r peopleimport.Run, m peopleimport.Mapping) peopleimport.Run {
	f.t.Helper()
	r, err := f.imports.Preview(f.ctx, f.actor, r.ID, m)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func (f *fixture) apply(r peopleimport.Run) peopleimport.Run {
	f.t.Helper()
	r, err := f.imports.Apply(f.ctx, f.actor, r.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}
func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(f.ctx, query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}
func assertDecision(t *testing.T, r peopleimport.Run, want ...string) {
	t.Helper()
	if len(r.Rows) != len(want) {
		t.Fatalf("rows=%d,want %d", len(r.Rows), len(want))
	}
	for i, v := range want {
		if r.Rows[i].Decision != v {
			t.Fatalf("row %d decision=%s, error=%s, want %s", r.Rows[i].Row, r.Rows[i].Decision, r.Rows[i].Error, v)
		}
	}
}

func TestGoodFixturesPreviewApplyAndIdempotentReimport(t *testing.T) {
	for _, name := range []string{"good.csv", "good.xlsx"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			run := f.upload(name)
			if _, err := f.imports.Apply(f.ctx, f.actor, run.ID); !errors.Is(err, peopleimport.ErrState) {
				t.Fatalf("apply before preview=%v", err)
			}
			run = f.preview(run, goodMapping())
			assertDecision(t, run, "create", "create", "create")
			if f.count("SELECT count(*) FROM people") != 1 || f.count("SELECT count(*) FROM invitations") != 0 || f.count("SELECT count(*) FROM teams") != 0 {
				t.Fatal("preview committed domain mutations")
			}
			encoded, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"classified-ada", "ada@home.test", "oli_", "Ada Lovelace"} {
				if bytes.Contains(encoded, []byte(secret)) {
					t.Fatal("source value disclosed in preview")
				}
			}
			var ciphertext []byte
			if err = f.pool.QueryRow(f.ctx, "SELECT payload_sealed FROM people_import_rows WHERE run_id=$1 ORDER BY row_number LIMIT 1", run.ID).Scan(&ciphertext); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(ciphertext, []byte("classified")) || bytes.Contains(ciphertext, []byte("ada@")) {
				t.Fatal("plaintext staging data")
			}
			run = f.apply(run)
			if run.State != "applied" {
				t.Fatal("not applied")
			}
			for _, row := range run.Rows {
				if row.Result != "created" {
					t.Fatalf("row=%+v", row)
				}
			}
			if f.count("SELECT count(*) FROM people WHERE status='invited'") != 3 || f.count("SELECT count(*) FROM invitations") != 3 || f.count("SELECT count(*) FROM team_memberships") != 3 {
				t.Fatal("missing people/invitations/team memberships")
			}
			if f.count("SELECT count(*) FROM profile_field_values WHERE value IS NULL AND value_sealed IS NOT NULL") != 3 {
				t.Fatal("sensitive values not centrally encrypted")
			}
			before := f.count("SELECT count(*) FROM audit_entries")
			_ = f.apply(run)
			if f.count("SELECT count(*) FROM audit_entries") != before {
				t.Fatal("repeated apply wrote audit or data")
			}
			reimport := f.preview(f.upload(name), goodMapping())
			assertDecision(t, reimport, "skip", "skip", "skip")
			_ = f.apply(reimport)
			if f.count("SELECT count(*) FROM people") != 4 || f.count("SELECT count(*) FROM invitations") != 3 {
				t.Fatal("reimport duplicated identity")
			}
		})
	}
}

func TestMessyMaliciousFixturesAndRowIsolation(t *testing.T) {
	f := newFixture(t)
	messy := f.preview(f.upload("messy.csv"), mapping("username", "work_email", "display_name"))
	assertDecision(t, messy, "create", "conflict")
	messy = f.apply(messy)
	if messy.Rows[0].Result != "created" || messy.Rows[1].Error != "column_count" {
		t.Fatalf("messy results=%+v", messy.Rows)
	}
	formulas := f.preview(f.upload("formula.xlsx"), goodMapping())
	assertDecision(t, formulas, "conflict", "create", "create")
	formulas = f.apply(formulas)
	if formulas.Rows[0].Error != "formula" || formulas.Rows[1].Result != "created" {
		t.Fatalf("formulas=%+v", formulas.Rows)
	}
	for _, name := range []string{"malformed.csv", "zip-bomb.xlsx", "zip-traversal.xlsx"} {
		file, err := fixtureFiles.Open("testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.imports.Upload(f.ctx, f.actor, peopleimport.FileSource{}, file)
		_ = file.Close()
		if !errors.Is(err, peopleimport.ErrFile) {
			t.Fatalf("%s error=%v", name, err)
		}
	}
	r := f.uploadText("username,work_email,display_name\ngood,good@acme.test,Good\nbad name,bad@acme.test,Bad\nlast,last@acme.test,Last\n")
	r = f.preview(r, mapping("username", "work_email", "display_name"))
	assertDecision(t, r, "create", "conflict", "create")
	r = f.apply(r)
	if r.Rows[0].Result != "created" || r.Rows[1].Result != "failed" || r.Rows[2].Result != "created" || f.count("SELECT count(*) FROM people WHERE username='bad name'") != 0 {
		t.Fatal("failed row affected other rows")
	}
}

func TestMatchingConflictsExternalIDAndUpdates(t *testing.T) {
	f := newFixture(t)
	_ = f.apply(f.preview(f.upload("good.csv"), goodMapping()))
	conflict := f.preview(f.uploadText("username,work_email,contact_email,display_name,external_id\nwrong,ada@acme.test,grace@home.test,Wrong,employee-1\n"), mapping("username", "work_email", "contact_email", "display_name", "external_id"))
	assertDecision(t, conflict, "conflict")
	if conflict.Rows[0].Error != "identity_conflict" {
		t.Fatal("disagreeing identities accepted")
	}
	dup := f.preview(f.uploadText("work_email,display_name\nada@acme.test,First\nADA@acme.test,Second\n"), mapping("work_email", "display_name"))
	assertDecision(t, dup, "conflict", "conflict")
	// Match by external ID alone, then safely update work email/name/manager.
	updated := f.preview(f.uploadText("external_id,work_email,display_name,manager_email\nemployee-1,ada.new@acme.test,Ada Updated,owner@acme.test\n"), mapping("external_id", "work_email", "display_name", "manager_email"))
	assertDecision(t, updated, "update")
	updated = f.apply(updated)
	if updated.Rows[0].Result != "updated" || f.count("SELECT count(*) FROM people WHERE work_email='ada.new@acme.test' AND display_name='Ada Updated' AND manager_id=$1", f.owner.ID) != 1 {
		t.Fatal("external match/base/manager update failed")
	}
	// Contact-only matching updates that same existing person without duplicates.
	contact := f.preview(f.uploadText("contact_email,title\nada@home.test,Principal Engineer\n"), mapping("contact_email", "title"))
	assertDecision(t, contact, "update")
	_ = f.apply(contact)
	if f.count("SELECT count(*) FROM people") != 4 {
		t.Fatal("contact match duplicated person")
	}
	// Existing invitation destinations cannot be redirected via a matched work email.
	redirect := f.preview(f.uploadText("work_email,contact_email\nada.new@acme.test,attacker@home.test\n"), mapping("work_email", "contact_email"))
	assertDecision(t, redirect, "conflict")
}

func TestPermissionsSensitiveFieldsAndGrantCeilings(t *testing.T) {
	f := newFixture(t)
	admin, err := f.ids.CreatePerson(t.Context(), audit.System, identity.NewPerson{Username: "admin", WorkEmail: "admin@acme.test", DisplayName: "Admin", Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.ids.AssignRole(t.Context(), audit.System, admin.ID, authz.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	perms, err := f.ids.PersonPermissions(t.Context(), admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	actor := audit.Actor{Kind: audit.ActorPerson, ID: admin.ID.String()}
	ctx := authz.WithPrincipal(t.Context(), &authz.Principal{Subject: admin.ID.String(), Permissions: perms})
	run, err := f.imports.Upload(ctx, actor, peopleimport.FileSource{}, strings.NewReader("work_email,role\nowner@acme.test,member\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err = f.imports.Preview(ctx, actor, run.ID, mapping("work_email", "role"))
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, run, "conflict")
	if run.Rows[0].Error != "permission_denied" {
		t.Fatal("lower caller can manage elevated target")
	}
	run, err = f.imports.Upload(ctx, actor, peopleimport.FileSource{}, strings.NewReader("username,work_email,display_name,role\nnewowner,newowner@acme.test,New Owner,owner\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err = f.imports.Preview(ctx, actor, run.ID, mapping("username", "work_email", "display_name", "role"))
	if err != nil {
		t.Fatal(err)
	}
	assertDecision(t, run, "conflict")
	secret, err := f.imports.Upload(ctx, actor, peopleimport.FileSource{}, strings.NewReader("work_email,employee_code\nowner@acme.test,secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.imports.Preview(ctx, actor, secret.ID, mapping("work_email", "profile.employee_code")); !errors.Is(err, peopleimport.ErrInvalid) {
		t.Fatalf("sensitive mapping=%v", err)
	}
	if _, err = f.imports.Get(f.ctx, f.actor, secret.ID); !errors.Is(err, peopleimport.ErrNotFound) {
		t.Fatalf("other uploader saw run: %v", err)
	}
	limited := authz.WithPrincipal(t.Context(), &authz.Principal{Subject: f.owner.ID.String(), Permissions: []authz.Permission{authz.InstanceRead}})
	if _, err = f.imports.Upload(limited, f.actor, peopleimport.FileSource{}, strings.NewReader("a\nb\n")); !errors.Is(err, peopleimport.ErrForbidden) {
		t.Fatalf("no people.manage upload: %v", err)
	}
}

func TestStalePreviewAndDefinitionChangesFailClosed(t *testing.T) {
	f := newFixture(t)
	_ = f.apply(f.preview(f.upload("good.csv"), goodMapping()))
	r := f.preview(f.uploadText("work_email,title\nada@acme.test,Changed\n"), mapping("work_email", "title"))
	assertDecision(t, r, "update")
	ada, err := f.ids.GetPersonByWorkEmail(t.Context(), "ada@acme.test")
	if err != nil {
		t.Fatal(err)
	}
	title := "Concurrent change"
	if _, err = f.ids.UpdatePerson(t.Context(), audit.System, ada.ID, identity.PersonUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	r = f.apply(r)
	if r.Rows[0].Error != "stale_preview" || r.Rows[0].Result != "failed" {
		t.Fatalf("stale mutation applied: %+v", r.Rows)
	}
}

func TestAuditRollbackPerRowAndWholeRun(t *testing.T) {
	f := newFixture(t)
	r := f.preview(f.uploadText("username,work_email,display_name\nfirst,first@acme.test,First\nsecond,second@acme.test,Second\n"), mapping("username", "work_email", "display_name"))
	fault := auditfault.Inject(t, f.pool, "NEW.action = 'person.created' AND NEW.metadata->>'status' = 'invited'", 1)
	r = f.apply(r)
	if r.Rows[0].Result != "failed" || r.Rows[1].Result != "failed" || f.count("SELECT count(*) FROM people") != 1 || f.count("SELECT count(*) FROM invitations") != 0 {
		t.Fatal("row audit failure left domain effects")
	}
	fault.Remove()
	good := f.preview(f.upload("good.csv"), goodMapping())
	fault = auditfault.Inject(t, f.pool, "NEW.action = 'import.applied'", 1)
	if _, err := f.imports.Apply(f.ctx, f.actor, good.ID); !auditfault.Is(err) {
		t.Fatalf("whole run audit failure=%v", err)
	}
	if f.count("SELECT count(*) FROM people") != 1 || f.count("SELECT count(*) FROM invitations") != 0 {
		t.Fatal("whole run audit failure committed person")
	}
	fault.Remove()
	good = f.apply(good)
	if good.Rows[0].Result != "created" {
		t.Fatal("rollback retry failed")
	}
}

func TestExplicitSkipAndMissingTeams(t *testing.T) {
	f := newFixture(t)
	m := goodMapping()
	m.CreateTeams = false
	r := f.preview(f.upload("good.csv"), m)
	assertDecision(t, r, "conflict", "conflict", "create")
	m = goodMapping()
	m.SkipRows = []int{2}
	r = f.preview(f.upload("good.csv"), m)
	assertDecision(t, r, "skip", "create", "create")
	_ = f.apply(r)
	if f.count("SELECT count(*) FROM people WHERE username='ada'") != 0 || f.count("SELECT count(*) FROM people") != 3 {
		t.Fatal("explicit skip mutated identity")
	}
}

func TestPreviewBindsTeamResolution(t *testing.T) {
	for _, tc := range []struct {
		name            string
		pending, absent bool
		change          string
	}{
		{name: "existing person replacement", change: "replace"},
		{name: "pending person replacement", pending: true, change: "replace"},
		{name: "renamed team would auto-create", change: "rename"},
		{name: "new ambiguity", change: "ambiguous"},
		{name: "archived reference", change: "archive"},
		{name: "pending absent team created externally", pending: true, absent: true, change: "create"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			var original store.Team
			var err error
			if !tc.absent {
				original, err = f.ids.CreateTeam(t.Context(), audit.System, identity.NewTeam{Slug: "engineering", Name: "Engineering"})
				if err != nil {
					t.Fatal(err)
				}
			}
			input := "username,work_email,display_name,teams\nowner,owner@acme.test,Owner,Engineering\n"
			if tc.pending {
				input = "username,work_email,display_name,teams\npending,pending@acme.test,Pending,Engineering\n"
			}
			run := f.preview(f.uploadText(input), mapping("username", "work_email", "display_name", "teams"))
			if tc.pending {
				assertDecision(t, run, "create")
			} else {
				assertDecision(t, run, "update")
			}
			if tc.change == "replace" || tc.change == "rename" {
				name := "Old Engineering"
				if _, err = f.ids.UpdateTeam(t.Context(), audit.System, original.ID, identity.TeamUpdate{Name: &name}); err != nil {
					t.Fatal(err)
				}
			}
			if tc.change == "archive" {
				if _, err = f.ids.ArchiveTeam(t.Context(), audit.System, original.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.change != "rename" && tc.change != "archive" {
				if _, err = f.ids.CreateTeam(t.Context(), audit.System, identity.NewTeam{Slug: "replacement", Name: "Engineering"}); err != nil {
					t.Fatal(err)
				}
			}
			run = f.apply(run)
			if run.Rows[0].Result != "failed" || run.Rows[0].Error != "stale_preview" {
				t.Fatalf("team drift was not rejected as stale: %+v", run.Rows)
			}
			if f.count("SELECT count(*) FROM team_memberships") != 0 {
				t.Fatal("stale team resolution granted membership")
			}
			if tc.pending && f.count("SELECT count(*) FROM people WHERE username='pending'") != 0 {
				t.Fatal("stale pending row created a person")
			}
		})
	}
}

func TestStableTeamResolutionAndSharedAutoCreation(t *testing.T) {
	t.Run("stable existing team", func(t *testing.T) {
		f := newFixture(t)
		team, err := f.ids.CreateTeam(t.Context(), audit.System, identity.NewTeam{Slug: "engineering", Name: "Engineering"})
		if err != nil {
			t.Fatal(err)
		}
		run := f.preview(f.uploadText("work_email,teams\nowner@acme.test,Engineering\n"), mapping("work_email", "teams"))
		assertDecision(t, run, "update")
		run = f.apply(run)
		if run.Rows[0].Result != "updated" || f.count("SELECT count(*) FROM team_memberships WHERE person_id=$1 AND team_id=$2", f.owner.ID, team.ID) != 1 {
			t.Fatalf("stable team was not applied: %+v", run.Rows)
		}
	})
	t.Run("stable absence shared across rows", func(t *testing.T) {
		f := newFixture(t)
		run := f.preview(f.uploadText("username,work_email,display_name,teams\nfirst,first@acme.test,First,Engineering\nsecond,second@acme.test,Second,engineering\n"), mapping("username", "work_email", "display_name", "teams"))
		assertDecision(t, run, "create", "create")
		if f.count("SELECT count(*) FROM teams") != 0 {
			t.Fatal("preview auto-created a persistent team")
		}
		run = f.apply(run)
		for _, row := range run.Rows {
			if row.Result != "created" {
				t.Fatalf("same-run auto-create became stale: %+v", run.Rows)
			}
		}
		if f.count("SELECT count(*) FROM teams WHERE lower(name)='engineering'") != 1 || f.count("SELECT count(DISTINCT team_id) FROM team_memberships") != 1 || f.count("SELECT count(*) FROM team_memberships") != 2 {
			t.Fatal("shared team auto-creation was not idempotent")
		}
	})
}

func TestCSVPhysicalLinesSurviveStagingAndExport(t *testing.T) {
	f := newFixture(t)
	run := f.preview(f.upload("physical-lines.csv"), mapping("username", "work_email", "display_name"))
	assertDecision(t, run, "create", "conflict", "create")
	for i, line := range []int{3, 6, 8} {
		if run.Rows[i].Row != line {
			t.Fatalf("preview row=%d,want original line%d", run.Rows[i].Row, line)
		}
	}
	run = f.apply(run)
	if run.Rows[0].Result != "created" || run.Rows[1].Result != "failed" || run.Rows[2].Result != "created" {
		t.Fatalf("row isolation=%+v", run.Rows)
	}
	if got := peopleimport.ErrorsCSV(run); got != "row,decision,error,result\n6,conflict,column_count,failed\n" {
		t.Fatalf("error export source line=%q", got)
	}
}
