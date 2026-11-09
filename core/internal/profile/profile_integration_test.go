//go:build integration

package profile_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/dbtest"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/profile"
	"github.com/orglane/orglane/tests/auditfault"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

type env struct {
	t        *testing.T
	pool     *pgxpool.Pool
	identity *identity.Service
	profile  *profile.Service
	keyring  *crypto.Keyring
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := database.Connect(ctx, config.Database{URL: pgtest.NewDatabase(t), MaxConns: 5, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Close() }()
	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	keyring, err := crypto.NewKeyring("profile-integration-test-master-secret-0123")
	if err != nil {
		t.Fatal(err)
	}
	identityService := identity.NewService(pool)
	return &env{
		t: t, pool: pool, identity: identityService, keyring: keyring,
		profile: profile.NewService(pool, keyring, profile.WithTeams(identityService)),
	}
}

var system = audit.Actor{Kind: audit.ActorSystem, IP: netip.MustParseAddr("203.0.113.9"), UserAgent: "orglane-test/1.0"}

func actorOf(v profile.Viewer) audit.Actor {
	return audit.Actor{Kind: audit.ActorPerson, ID: v.PersonID.String()}
}

func (e *env) person(username string) profile.Viewer {
	e.t.Helper()
	p, err := e.identity.CreatePerson(context.Background(), system, identity.NewPerson{
		Username: username, WorkEmail: username + "@acme.test", DisplayName: username, Status: identity.StatusActive,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return profile.Viewer{PersonID: p.ID}
}

func (e *env) field(in profile.NewField) profile.Field {
	e.t.Helper()
	f, err := e.profile.CreateField(context.Background(), system, in)
	if err != nil {
		e.t.Fatalf("CreateField(%s): %v", in.Label, err)
	}
	return f
}

func (e *env) library(key string) profile.Field {
	e.t.Helper()
	f, _, err := e.profile.EnableLibraryField(context.Background(), system, key)
	if err != nil {
		e.t.Fatalf("EnableLibraryField(%s): %v", key, err)
	}
	return f
}

func (e *env) set(v profile.Viewer, person uuid.UUID, kv ...string) []profile.Entry {
	e.t.Helper()
	changes := make([]profile.Change, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		changes = append(changes, profile.Change{Key: kv[i], Value: &kv[i+1]})
	}
	entries, err := e.profile.UpdateProfile(context.Background(), actorOf(v), v, person, changes)
	if err != nil {
		e.t.Fatalf("UpdateProfile(%v): %v", kv, err)
	}
	return entries
}

// view returns what v sees of person's profile: field key → value ("" for
// a visible field without a visible value).
func (e *env) view(v profile.Viewer, person uuid.UUID) map[string]string {
	e.t.Helper()
	entries, err := e.profile.Profile(context.Background(), actorOf(v), v, person)
	if err != nil {
		e.t.Fatalf("Profile: %v", err)
	}
	out := make(map[string]string, len(entries))
	for _, en := range entries {
		out[en.Field.Key] = en.Value
	}
	return out
}

func (e *env) entry(v profile.Viewer, person uuid.UUID, key string) (profile.Entry, bool) {
	e.t.Helper()
	entries, err := e.profile.Profile(context.Background(), actorOf(v), v, person)
	if err != nil {
		e.t.Fatalf("Profile: %v", err)
	}
	i := slices.IndexFunc(entries, func(en profile.Entry) bool { return en.Field.Key == key })
	if i < 0 {
		return profile.Entry{}, false
	}
	return entries[i], true
}

func (e *env) auditCount(action audit.Action, target uuid.UUID) int {
	e.t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE action = $1 AND target_id = $2`, string(action), target.String()).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) lastAuditMetadata(action audit.Action, target uuid.UUID) map[string]any {
	e.t.Helper()
	var raw []byte
	var actorID *string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT metadata, actor_id FROM audit_entries WHERE action = $1 AND target_id = $2 ORDER BY id DESC LIMIT 1`,
		string(action), target.String()).Scan(&raw, &actorID); err != nil {
		e.t.Fatalf("audit entry %s: %v", action, err)
	}
	var md map[string]any
	if err := json.Unmarshal(raw, &md); err != nil {
		e.t.Fatal(err)
	}
	if actorID != nil {
		md["@actor"] = *actorID
	}
	return md
}

// TestVisibilityMatrix checks what each kind of viewer sees of one
// profile: the person, a teammate, another member, an admin and an admin
// with people.sensitive.read.
func TestVisibilityMatrix(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	teammate := e.person("teammate")
	other := e.person("other")
	admin := e.person("admin")
	admin.ManagePeople = true
	sensitiveAdmin := e.person("owner")
	sensitiveAdmin.ManagePeople, sensitiveAdmin.ReadSensitive = true, true
	e.team("platform", subject, teammate)
	e.team("sales", other)

	e.library("telegram")          // everyone
	e.library("phone")             // team
	e.library("tshirt_size")       // admins
	e.library("emergency_contact") // admins, sensitive
	e.field(profile.NewField{Label: "Desk", Type: profile.TypeText, Visibility: profile.VisibilityEveryone, Sensitive: true})
	e.set(sensitiveAdmin, subject.PersonID,
		"telegram", "@subject_tg", "phone", "+49 30 1234567", "tshirt_size", "m",
		"emergency_contact", "Sam, +49 30 7654321", "desk", "B-12")

	all := map[string]string{
		"telegram": "subject_tg", "phone": "+49301234567", "tshirt_size": "m",
		"emergency_contact": "Sam, +49 30 7654321", "desk": "B-12",
	}
	want := map[string]map[string]string{
		"self":            {"telegram": "subject_tg", "phone": "+49301234567", "tshirt_size": "m"},
		"teammate":        {"telegram": "subject_tg", "phone": "+49301234567"},
		"other member":    {"telegram": "subject_tg"},
		"admin":           {"telegram": "subject_tg", "phone": "+49301234567", "tshirt_size": "m"},
		"sensitive admin": all,
	}
	for name, v := range map[string]profile.Viewer{
		"self": subject, "teammate": teammate, "other member": other, "admin": admin, "sensitive admin": sensitiveAdmin,
	} {
		if got := e.view(v, subject.PersonID); !maps.Equal(got, want[name]) {
			t.Errorf("%s sees %v, want %v", name, got, want[name])
		}
	}

	// Archived fields disappear for everyone, values included, and come
	// back when restored.
	fields, err := e.profile.Fields(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	telegram := fields[slices.IndexFunc(fields, func(f profile.Field) bool { return f.Key == "telegram" })]
	archive := true
	if _, err := e.profile.UpdateField(t.Context(), system, profile.Viewer{ReadSensitive: true}, telegram.ID, profile.FieldUpdate{Archived: &archive}); err != nil {
		t.Fatal(err)
	}
	if got := e.view(sensitiveAdmin, subject.PersonID); got["telegram"] != "" || len(got) != 4 {
		t.Errorf("archived field still shown: %v", got)
	}
	if _, err := e.profile.UpdateProfile(t.Context(), actorOf(subject), subject, subject.PersonID,
		[]profile.Change{{Key: "telegram", Value: new("x")}}); !errors.Is(err, profile.ErrInvalid) {
		t.Errorf("changing an archived field: err = %v, want ErrInvalid", err)
	}
	if f := e.library("telegram"); f.Archived || f.ID != telegram.ID {
		t.Errorf("enabling the library field again did not restore it: %+v", f)
	}
	if got := e.view(other, subject.PersonID); got["telegram"] != "subject_tg" {
		t.Errorf("restored field: %v", got)
	}

	// Offboarded people are only found by admins.
	if _, err := e.identity.SetPersonStatus(t.Context(), system, subject.PersonID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	if _, err := e.profile.Profile(t.Context(), actorOf(other), other, subject.PersonID); !errors.Is(err, profile.ErrNotFound) {
		t.Errorf("offboarded profile for a member: err = %v", err)
	}
	if got := e.view(admin, subject.PersonID); got["telegram"] != "subject_tg" {
		t.Errorf("offboarded profile for an admin: %v", got)
	}
	if _, err := e.profile.Profile(t.Context(), actorOf(admin), admin, uuid.New()); !errors.Is(err, profile.ErrNotFound) {
		t.Errorf("unknown person: err = %v", err)
	}
}

func TestUserHide(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	other := e.person("other")
	admin := e.person("admin")
	admin.ManagePeople = true
	e.library("telegram") // the user can hide it
	e.library("location") // too
	e.field(profile.NewField{Label: "Office", Type: profile.TypeText})
	e.set(subject, subject.PersonID, "telegram", "subject_tg", "location", "Lisbon", "office", "HQ")

	hide := func(v profile.Viewer, key string, hidden bool) error {
		_, err := e.profile.UpdateProfile(t.Context(), actorOf(v), v, subject.PersonID, []profile.Change{{Key: key, Hidden: &hidden}})
		return err
	}
	if err := hide(subject, "telegram", true); err != nil {
		t.Fatal(err)
	}
	// Others see the field, but not the value or that it is hidden.
	en, ok := e.entry(other, subject.PersonID, "telegram")
	if !ok || en.Value != "" || en.Hidden {
		t.Errorf("other member sees a hidden value: %+v", en)
	}
	for name, v := range map[string]profile.Viewer{"self": subject, "admin": admin} {
		en, _ := e.entry(v, subject.PersonID, "telegram")
		if en.Value != "subject_tg" || !en.Hidden {
			t.Errorf("%s: %+v, want the value marked hidden", name, en)
		}
	}
	if en, _ := e.entry(subject, subject.PersonID, "telegram"); !en.Hideable {
		t.Error("telegram is not hideable for the person")
	}
	if en, _ := e.entry(admin, subject.PersonID, "telegram"); en.Hideable {
		t.Error("telegram is hideable for an admin")
	}

	// Fields that do not allow it cannot be hidden; only the person hides.
	if err := hide(subject, "office", true); !errors.Is(err, profile.ErrForbidden) {
		t.Errorf("hiding a field that does not allow it: err = %v", err)
	}
	if err := hide(admin, "location", true); !errors.Is(err, profile.ErrForbidden) {
		t.Errorf("an admin hides a value: err = %v", err)
	}
	if err := hide(admin, "telegram", false); !errors.Is(err, profile.ErrForbidden) {
		t.Errorf("an admin shows a hidden value: err = %v", err)
	}
	// An admin edits a hidden value; it stays hidden.
	e.set(admin, subject.PersonID, "telegram", "subject_tg2")
	if en, _ := e.entry(subject, subject.PersonID, "telegram"); en.Value != "subject_tg2" || !en.Hidden {
		t.Errorf("after an admin's edit: %+v", en)
	}
	// Even when the field stops allowing it, a hidden value stays hidden
	// until the person shows it.
	fields, _ := e.profile.Fields(t.Context(), false)
	tg := fields[slices.IndexFunc(fields, func(f profile.Field) bool { return f.Key == "telegram" })]
	no := false
	if _, err := e.profile.UpdateField(t.Context(), system, profile.Viewer{ReadSensitive: true}, tg.ID, profile.FieldUpdate{UserCanHide: &no}); err != nil {
		t.Fatal(err)
	}
	if got := e.view(other, subject.PersonID); got["telegram"] != "" {
		t.Errorf("hidden value shown after the field stopped allowing hiding: %v", got)
	}
	if err := hide(subject, "telegram", false); err != nil {
		t.Fatal(err)
	}
	if got := e.view(other, subject.PersonID); got["telegram"] != "subject_tg2" {
		t.Errorf("after showing it again: %v", got)
	}
}

func TestSensitiveValuesAreEncrypted(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	second := e.person("second")
	owner := e.person("owner")
	owner.ManagePeople, owner.ReadSensitive = true, true
	ec := e.library("emergency_contact")
	const contact = "Sam Doe, +49 30 7654321"
	e.set(owner, subject.PersonID, "emergency_contact", contact)
	e.set(owner, second.PersonID, "emergency_contact", "Someone else")

	raw := func(person uuid.UUID) (value *string, sealed []byte) {
		t.Helper()
		if err := e.pool.QueryRow(t.Context(), `SELECT value, value_sealed FROM profile_field_values WHERE person_id = $1 AND field_id = $2`,
			person, ec.ID).Scan(&value, &sealed); err != nil {
			t.Fatal(err)
		}
		return value, sealed
	}
	value, sealed := raw(subject.PersonID)
	if value != nil || len(sealed) == 0 || bytes.Contains(sealed, []byte("Sam")) {
		t.Fatalf("the database holds value=%v sealed=%x, want only ciphertext", value, sealed)
	}
	// The ciphertext opens with the profile-fields key only, bound to its row.
	plain, err := e.keyring.Sealer(crypto.PurposeProfileFields).Open(sealed, rowAD(subject.PersonID, ec.ID))
	if err != nil || string(plain) != contact {
		t.Fatalf("open with the derived key: %q, %v", plain, err)
	}
	if _, err := e.keyring.Sealer(crypto.PurposeOIDCTokens).Open(sealed, rowAD(subject.PersonID, ec.ID)); err == nil {
		t.Fatal("the value opens with another purpose's key")
	}
	// A ciphertext moved to another person's row does not open.
	if _, err := e.pool.Exec(t.Context(), `UPDATE profile_field_values SET value_sealed = $1 WHERE person_id = $2 AND field_id = $3`,
		sealed, second.PersonID, ec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.profile.Profile(t.Context(), actorOf(owner), owner, second.PersonID); !errors.Is(err, crypto.ErrDecrypt) {
		t.Errorf("swapped ciphertext: err = %v, want ErrDecrypt", err)
	}
	if _, err := e.pool.Exec(t.Context(), `DELETE FROM profile_field_values WHERE person_id = $1`, second.PersonID); err != nil {
		t.Fatal(err)
	}

	// Turning sensitivity off decrypts the values in place; on encrypts
	// them again.
	off, on := false, true
	if _, err := e.profile.UpdateField(t.Context(), system, profile.Viewer{ReadSensitive: true}, ec.ID, profile.FieldUpdate{Sensitive: &off}); err != nil {
		t.Fatal(err)
	}
	if value, sealed := raw(subject.PersonID); value == nil || *value != contact || sealed != nil {
		t.Errorf("after turning sensitivity off: value=%v sealed=%x", value, sealed)
	}
	if _, err := e.profile.UpdateField(t.Context(), system, profile.Viewer{ReadSensitive: true}, ec.ID, profile.FieldUpdate{Sensitive: &on}); err != nil {
		t.Fatal(err)
	}
	if value, sealed := raw(subject.PersonID); value != nil || len(sealed) == 0 {
		t.Errorf("after turning sensitivity on: value=%v sealed=%x", value, sealed)
	}
	if got := e.view(owner, subject.PersonID); got["emergency_contact"] != contact {
		t.Errorf("owner reads %v", got)
	}
}

func rowAD(person, field uuid.UUID) []byte {
	return append(append([]byte("profile_field_value:"), person[:]...), field[:]...)
}

func TestSensitiveReadsAreAudited(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	member := e.person("member")
	admin := e.person("admin")
	admin.ManagePeople = true
	owner := e.person("owner")
	owner.ManagePeople, owner.ReadSensitive = true, true
	e.library("emergency_contact")
	e.library("telegram")
	e.set(subject, subject.PersonID, "telegram", "subject_tg")

	// No sensitive value yet: nothing to audit.
	e.view(owner, subject.PersonID)
	if n := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); n != 0 {
		t.Fatalf("%d sensitive read entries without a sensitive value", n)
	}
	e.set(owner, subject.PersonID, "emergency_contact", "Sam")
	before := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID) // the response to the change itself
	if before != 1 {
		t.Fatalf("the change's response was audited %d times, want 1", before)
	}

	// Viewers who cannot read the value do not cause an entry.
	e.view(member, subject.PersonID)
	e.view(admin, subject.PersonID)
	if n := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); n != before {
		t.Errorf("reads without the sensitive value were audited: %d", n-before)
	}
	// Every authorized read is audited; even self needs sensitive access.
	e.view(owner, subject.PersonID)
	e.view(owner, subject.PersonID)
	md := e.lastAuditMetadata(audit.PersonSensitiveFieldsRead, subject.PersonID)
	if md["@actor"] != owner.PersonID.String() || !slices.Equal(anyStrings(md["fields"]), []string{"emergency_contact"}) {
		t.Errorf("audit entry metadata: %v", md)
	}
	e.view(subject, subject.PersonID)
	if n := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); n != before+2 {
		t.Errorf("sensitive reads audited %d times, want %d", n-before, 2)
	}
	// The entries never hold the value.
	var leaked int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_entries WHERE metadata::text LIKE '%Sam%'`).Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Errorf("%d audit entries contain the sensitive value", leaked)
	}
}

func anyStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, s := range list {
		str, _ := s.(string)
		out = append(out, str)
	}
	return out
}

func TestBirthdayYear(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	member := e.person("member")
	admin := e.person("admin")
	admin.ManagePeople = true
	owner := e.person("owner")
	owner.ManagePeople, owner.ReadSensitive = true, true
	e.library("birthday")
	e.field(profile.NewField{Label: "Anniversary", Type: profile.TypeDate})
	e.set(subject, subject.PersonID, "birthday", "1990-03-14", "anniversary", "2020-06-01")

	for name, tc := range map[string]struct {
		v          profile.Viewer
		want       string
		yearHidden bool
	}{
		"self":            {subject, "1990-03-14", false},
		"member":          {member, "--03-14", true},
		"admin":           {admin, "--03-14", true},
		"sensitive admin": {owner, "1990-03-14", false},
	} {
		en, ok := e.entry(tc.v, subject.PersonID, "birthday")
		if !ok || en.Value != tc.want || en.YearHidden != tc.yearHidden {
			t.Errorf("%s: birthday %+v, want %q", name, en, tc.want)
		}
		if got := e.view(tc.v, subject.PersonID)["anniversary"]; got != "2020-06-01" {
			t.Errorf("%s: other dates keep their year: %q", name, got)
		}
	}
	// Marking birthday sensitive removes even self access without the scope.
	birthday := e.library("birthday")
	if _, err := e.profile.UpdateField(t.Context(), actorOf(owner), owner, birthday.ID, profile.FieldUpdate{Sensitive: new(true)}); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []profile.Viewer{subject, member, admin} {
		if _, ok := e.entry(viewer, subject.PersonID, "birthday"); ok {
			t.Error("sensitive birthday bypassed permission gate")
		}
	}
	if entry, ok := e.entry(owner, subject.PersonID, "birthday"); !ok || entry.Value != "1990-03-14" {
		t.Errorf("authorized sensitive birthday: %+v", entry)
	}
}

func TestOrdinaryBirthdayReadsDoNotAuditSensitiveValues(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	e.library("birthday")
	e.set(subject, subject.PersonID, "birthday", "1990-03-14")
	e.view(subject, subject.PersonID)
	// Birthdays are not sensitive: reading them is not audited.
	if n := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); n != 0 {
		t.Errorf("birthday reads audited: %d", n)
	}
}

func TestValidationPerType(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	subject.ReadSensitive = true // exercise validation of every type, including sensitive long text
	e.library("telegram")
	e.library("github")
	e.library("phone")
	e.library("birthday")
	e.library("tshirt_size")
	e.library("emergency_contact")
	e.field(profile.NewField{Label: "Website", Type: profile.TypeURL})
	e.field(profile.NewField{Label: "Bio", Type: profile.TypeLongText})
	e.field(profile.NewField{Label: "Nickname", Type: profile.TypeText})
	e.field(profile.NewField{Label: "Mastodon", Type: profile.TypeHandle, HandleKind: profile.HandleMastodon})
	e.field(profile.NewField{Label: "Office", Type: profile.TypeSelect, Options: []profile.Option{{Label: "Berlin"}, {Label: "Lisbon"}}})

	valid := e.set(subject, subject.PersonID,
		"telegram", "@maya_okafor", "github", "maya-okafor", "phone", "+1 (555) 010-4477", "birthday", "1990-03-14",
		"tshirt_size", "xl", "emergency_contact", "Sam\r\n+1 555 0100", "website", "https://example.com/maya",
		"bio", "Robots.\nAnd tea.", "nickname", " Maya ", "mastodon", "@maya@Robots.Example", "office", "lisbon")
	got := map[string]string{}
	for _, en := range valid {
		got[en.Field.Key] = en.Value
	}
	want := map[string]string{
		"telegram": "maya_okafor", "github": "maya-okafor", "phone": "+15550104477", "birthday": "1990-03-14",
		"tshirt_size": "xl", "emergency_contact": "Sam\n+1 555 0100", "website": "https://example.com/maya",
		"bio": "Robots.\nAnd tea.", "nickname": "Maya", "mastodon": "maya@robots.example", "office": "lisbon",
	}
	if !maps.Equal(got, want) {
		t.Errorf("stored %v, want %v", got, want)
	}

	for key, value := range map[string]string{
		"telegram":    "no",
		"github":      "-maya",
		"phone":       "555-0100",
		"birthday":    "1990-02-30",
		"tshirt_size": "huge",
		"website":     "javascript:alert(1)",
		"nickname":    "two\nlines",
		"mastodon":    "maya",
		"office":      "Paris",
	} {
		// One bad value rejects the whole change.
		_, err := e.profile.UpdateProfile(t.Context(), actorOf(subject), subject, subject.PersonID, []profile.Change{
			{Key: "bio", Value: new("Changed")}, {Key: key, Value: &value},
		})
		if !errors.Is(err, profile.ErrInvalid) {
			t.Errorf("%s = %q: err = %v, want ErrInvalid", key, value, err)
		}
	}
	if got := e.view(subject, subject.PersonID); got["bio"] != "Robots.\nAnd tea." {
		t.Errorf("a rejected change was partly applied: bio = %q", got["bio"])
	}

	// Unknown fields and duplicates are rejected; an empty value clears.
	for name, changes := range map[string][]profile.Change{
		"unknown":   {{Key: "nope", Value: new("x")}},
		"duplicate": {{Key: "bio", Value: new("a")}, {Key: "bio", Value: new("b")}},
	} {
		if _, err := e.profile.UpdateProfile(t.Context(), actorOf(subject), subject, subject.PersonID, changes); !errors.Is(err, profile.ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	e.set(subject, subject.PersonID, "bio", "")
	if en, _ := e.entry(subject, subject.PersonID, "bio"); en.Value != "" {
		t.Errorf("bio not cleared: %+v", en)
	}
	var n int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM profile_field_values WHERE person_id = $1`, subject.PersonID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != len(want)-1 {
		t.Errorf("%d stored values, want %d", n, len(want)-1)
	}
}

func TestEditRules(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	member := e.person("member")
	admin := e.person("admin")
	admin.ManagePeople = true
	e.library("telegram")   // editable by self
	e.library("start_date") // editable by admins
	e.library("emergency_contact")

	change := func(v profile.Viewer, key, value string) error {
		_, err := e.profile.UpdateProfile(t.Context(), actorOf(v), v, subject.PersonID, []profile.Change{{Key: key, Value: &value}})
		return err
	}
	if err := change(member, "telegram", "member_tg"); !errors.Is(err, profile.ErrForbidden) {
		t.Errorf("a member changes someone else's profile: err = %v", err)
	}
	if err := change(subject, "start_date", "2024-01-15"); !errors.Is(err, profile.ErrForbidden) {
		t.Errorf("the person changes an admin-only field: err = %v", err)
	}
	if err := change(admin, "start_date", "2024-01-15"); err != nil {
		t.Errorf("an admin changes an admin-only field: %v", err)
	}
	if err := change(admin, "telegram", "set_by_admin"); err != nil {
		t.Errorf("an admin changes a self-editable field: %v", err)
	}
	// A sensitive field is unknown to an admin who cannot read it.
	if err := change(admin, "emergency_contact", "x"); !errors.Is(err, profile.ErrInvalid) {
		t.Errorf("an admin without people.sensitive.read changes a sensitive field: err = %v", err)
	}
	if err := change(subject, "emergency_contact", "Sam"); !errors.Is(err, profile.ErrInvalid) {
		t.Errorf("self without sensitive-read changes sensitive field: %v", err)
	}
	// Ordinary self edits remain available and audited.
	if err := change(subject, "telegram", "subject_tg"); err != nil {
		t.Fatal(err)
	}
	en, _ := e.entry(subject, subject.PersonID, "start_date")
	if en.Value != "2024-01-15" || en.Editable {
		t.Errorf("start date for the person: %+v", en)
	}
	if en, _ := e.entry(member, subject.PersonID, "telegram"); en.Editable {
		t.Error("telegram is editable for another member")
	}

	// Changes are audited with field keys only.
	md := e.lastAuditMetadata(audit.PersonProfileUpdated, subject.PersonID)
	if !slices.Equal(anyStrings(md["fields"]), []string{"telegram"}) || md["@actor"] != subject.PersonID.String() {
		t.Errorf("profile update audit: %v", md)
	}
	before := e.auditCount(audit.PersonProfileUpdated, subject.PersonID)
	if err := change(subject, "telegram", " subject_tg "); err != nil { // same value after normalization
		t.Fatal(err)
	}
	if n := e.auditCount(audit.PersonProfileUpdated, subject.PersonID); n != before {
		t.Errorf("a no-op change was audited")
	}
}

func TestFieldDefinitions(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	ctx := t.Context()

	office := e.field(profile.NewField{Label: "Office location", Type: profile.TypeSelect,
		Options: []profile.Option{{Label: "Berlin"}, {Label: "Lisbon"}}, Visibility: profile.VisibilityTeam})
	if office.Key != "office_location" || office.Section != profile.SectionCustom || office.EditableBy != profile.EditableBySelf || office.Library {
		t.Errorf("defaults: %+v", office)
	}
	if again := e.field(profile.NewField{Label: "Office location", Type: profile.TypeText}); again.Key != "office_location_2" {
		t.Errorf("second derived key: %q", again.Key)
	}
	for name, in := range map[string]profile.NewField{
		"taken key":           {Key: "office_location", Label: "X", Type: profile.TypeText},
		"reserved key":        {Key: "telegram", Label: "My Telegram", Type: profile.TypeText},
		"bad key":             {Key: "Bad Key", Label: "X", Type: profile.TypeText},
		"no label":            {Label: " ", Type: profile.TypeText},
		"bad type":            {Label: "X", Type: "number"},
		"handle without kind": {Label: "X", Type: profile.TypeHandle},
		"kind without handle": {Label: "X", Type: profile.TypeText, HandleKind: profile.HandleGitHub},
		"select without opts": {Label: "X", Type: profile.TypeSelect},
		"bad visibility":      {Label: "X", Type: profile.TypeText, Visibility: "public"},
	} {
		_, err := e.profile.CreateField(ctx, system, in)
		if !errors.Is(err, profile.ErrInvalid) && !errors.Is(err, profile.ErrKeyTaken) {
			t.Errorf("%s: err = %v", name, err)
		}
	}

	// Options in use cannot be removed; others can, and labels change.
	e.set(subject, subject.PersonID, "office_location", "lisbon")
	_, err := e.profile.UpdateField(ctx, system, profile.Viewer{ReadSensitive: true}, office.ID, profile.FieldUpdate{Options: &[]profile.Option{{Key: "berlin", Label: "Berlin"}}})
	if !errors.Is(err, profile.ErrOptionInUse) {
		t.Errorf("removing an option in use: err = %v", err)
	}
	updated, err := e.profile.UpdateField(ctx, system, profile.Viewer{ReadSensitive: true}, office.ID, profile.FieldUpdate{
		Label:   new("Office"),
		Options: &[]profile.Option{{Key: "lisbon", Label: "Lisboa"}, {Label: "Porto"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Label != "Office" || len(updated.Options) != 2 || updated.Options[1].Key != "porto" {
		t.Errorf("updated field: %+v", updated)
	}
	md := e.lastAuditMetadata(audit.ProfileFieldUpdated, office.ID)
	if !slices.Equal(anyStrings(md["changes"]), []string{"label", "options"}) {
		t.Errorf("field update audit: %v", md)
	}

	// Library fields keep their label and options, but settings change.
	shirt := e.library("tshirt_size")
	if _, err := e.profile.UpdateField(ctx, system, profile.Viewer{ReadSensitive: true}, shirt.ID, profile.FieldUpdate{Label: new("Shirt")}); !errors.Is(err, profile.ErrInvalid) {
		t.Errorf("renaming a library field: err = %v", err)
	}
	vis := profile.VisibilityEveryone
	if f, err := e.profile.UpdateField(ctx, system, profile.Viewer{ReadSensitive: true}, shirt.ID, profile.FieldUpdate{Visibility: &vis, Position: new(0)}); err != nil || f.Visibility != vis || f.Position != 0 {
		t.Errorf("changing library field settings: %+v, %v", f, err)
	}
	if _, _, err := e.profile.EnableLibraryField(ctx, system, "favorite_color"); !errors.Is(err, profile.ErrNotFound) {
		t.Errorf("unknown library field: err = %v", err)
	}
	if _, created, err := e.profile.EnableLibraryField(ctx, system, "tshirt_size"); err != nil || created {
		t.Errorf("enabling an enabled field: created=%v err=%v", created, err)
	}
	if _, err := e.profile.UpdateField(ctx, system, profile.Viewer{ReadSensitive: true}, uuid.New(), profile.FieldUpdate{}); !errors.Is(err, profile.ErrNotFound) {
		t.Errorf("unknown field: err = %v", err)
	}

	// Listing: by section, then position; archived only on request.
	e.library("telegram")
	archive := true
	if _, err := e.profile.UpdateField(ctx, system, profile.Viewer{ReadSensitive: true}, updated.ID, profile.FieldUpdate{Archived: &archive}); err != nil {
		t.Fatal(err)
	}
	active, _ := e.profile.Fields(ctx, false)
	all, _ := e.profile.Fields(ctx, true)
	keys := func(fs []profile.Field) []string {
		out := make([]string, 0, len(fs))
		for _, f := range fs {
			out = append(out, f.Key)
		}
		return out
	}
	if got := keys(active); !slices.Equal(got, []string{"telegram", "tshirt_size", "office_location_2"}) {
		t.Errorf("active fields: %v", got)
	}
	if got := keys(all); len(got) != 4 || !slices.Contains(got, "office_location") {
		t.Errorf("all fields: %v", got)
	}
	if n := e.auditCount(audit.ProfileFieldCreated, office.ID); n != 1 {
		t.Errorf("field creation audited %d times", n)
	}
}

// Definition changes must never let people.manage bypass the sensitive scope.
func TestSensitiveDeclassificationRequiresPermission(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	admin := e.person("admin")
	admin.ManagePeople = true
	owner := e.person("owner")
	owner.ManagePeople, owner.ReadSensitive = true, true
	field := e.field(profile.NewField{Label: "Private choice", Type: profile.TypeSelect, Sensitive: true, Options: []profile.Option{{Key: "a", Label: "A"}, {Key: "b", Label: "B"}}})
	e.set(owner, subject.PersonID, field.Key, "a")
	off := false
	if _, err := e.profile.UpdateField(t.Context(), actorOf(admin), admin, field.ID, profile.FieldUpdate{Sensitive: &off}); !errors.Is(err, profile.ErrForbidden) {
		t.Fatalf("declassification without sensitive scope: %v", err)
	}
	options := []profile.Option{{Key: "b", Label: "B"}}
	if _, err := e.profile.UpdateField(t.Context(), actorOf(admin), admin, field.ID, profile.FieldUpdate{Options: &options}); !errors.Is(err, profile.ErrForbidden) {
		t.Fatalf("sensitive option probe: %v", err)
	}
	everyone := profile.VisibilityEveryone
	if _, err := e.profile.UpdateField(t.Context(), actorOf(admin), admin, field.ID, profile.FieldUpdate{Visibility: &everyone}); err != nil {
		t.Fatal(err)
	}
	for _, viewer := range []profile.Viewer{subject, admin} {
		if got := e.view(viewer, subject.PersonID); len(got) != 0 {
			t.Errorf("unauthorized viewer sees sensitive values: %v", got)
		}
		if _, err := e.profile.UpdateProfile(t.Context(), actorOf(viewer), viewer, subject.PersonID, []profile.Change{{Key: field.Key, Value: new("b")}}); !errors.Is(err, profile.ErrInvalid) {
			t.Errorf("unauthorized write: %v", err)
		}
	}
	before := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID)
	if _, err := e.profile.UpdateField(t.Context(), actorOf(owner), owner, field.ID, profile.FieldUpdate{Options: &options, Sensitive: &off}); !errors.Is(err, profile.ErrOptionInUse) {
		t.Fatalf("authorized option conflict: %v", err)
	}
	if count := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); count != before+1 {
		t.Errorf("conflict read audited %d times", count-before)
	}
	if value := e.view(owner, subject.PersonID)[field.Key]; value != "a" {
		t.Errorf("conflict changed sensitive value: %q", value)
	}
	before = e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID)
	if _, err := e.profile.UpdateField(t.Context(), actorOf(owner), owner, field.ID, profile.FieldUpdate{Sensitive: &off}); err != nil {
		t.Fatal(err)
	}
	if count := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); count != before+1 {
		t.Errorf("declassification read audited %d times", count-before)
	}
	if got := e.view(admin, subject.PersonID)[field.Key]; got != "a" {
		t.Errorf("authorized declassification: %q", got)
	}
}

func TestSensitiveReadRequiresCommittedAudit(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	owner := e.person("owner")
	owner.ManagePeople, owner.ReadSensitive = true, true
	field := e.library("emergency_contact")
	e.set(owner, subject.PersonID, field.Key, "Sam")
	fault := auditfault.Inject(t, e.pool, "NEW.action = '"+string(audit.PersonSensitiveFieldsRead)+"'", 1)
	entries, err := e.profile.Profile(t.Context(), actorOf(owner), owner, subject.PersonID)
	if !auditfault.Is(err) || entries != nil {
		t.Fatalf("sensitive read with rejected audit returned entries=%v err=%v", entries, err)
	}
	if _, err := e.profile.UpdateField(t.Context(), actorOf(owner), owner, field.ID, profile.FieldUpdate{Sensitive: new(false)}); !auditfault.Is(err) {
		t.Fatalf("declassification without committed read audit: %v", err)
	}
	var sensitive, ciphertext bool
	if err := e.pool.QueryRow(t.Context(), `SELECT f.sensitive, v.value IS NULL AND v.value_sealed IS NOT NULL FROM profile_fields f JOIN profile_field_values v ON v.field_id = f.id WHERE f.id = $1 AND v.person_id = $2`, field.ID, subject.PersonID).Scan(&sensitive, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if !sensitive || !ciphertext {
		t.Error("rejected declassification changed stored sensitivity or encryption")
	}
	if hits := fault.Hits(); hits != 2 {
		t.Errorf("audit failure reached %d times, want 2", hits)
	}
	fault.Remove()
	if got := e.view(owner, subject.PersonID)[field.Key]; got != "Sam" {
		t.Errorf("authorized audited read: %q", got)
	}
}

func (e *env) team(slug string, members ...profile.Viewer) uuid.UUID {
	e.t.Helper()
	team, err := e.identity.CreateTeam(e.t.Context(), system, identity.NewTeam{Slug: slug, Name: slug})
	if err != nil {
		e.t.Fatal(err)
	}
	for _, person := range members {
		if _, err := e.identity.SetTeamMember(e.t.Context(), system, team.ID, person.PersonID, identity.TeamRoleMember); err != nil {
			e.t.Fatal(err)
		}
	}
	return team.ID
}

func TestTeamVisibilityUsesActiveMemberships(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	teammate := e.person("teammate")
	outsider := e.person("outsider")
	admin := e.person("admin")
	admin.ManagePeople = true
	first := e.team("first", subject, teammate)
	second := e.team("second", subject, teammate)
	e.team("other", outsider)
	field := e.field(profile.NewField{Label: "Team note", Type: profile.TypeText, Visibility: profile.VisibilityTeam})
	e.set(subject, subject.PersonID, field.Key, "Shared with my teams")
	if got := e.view(teammate, subject.PersonID)[field.Key]; got != "Shared with my teams" {
		t.Error("active teammate cannot see team field")
	}
	if _, ok := e.entry(outsider, subject.PersonID, field.Key); ok {
		t.Error("other member sees team field")
	}
	if _, err := e.identity.ArchiveTeam(t.Context(), system, first); err != nil {
		t.Fatal(err)
	}
	if got := e.view(teammate, subject.PersonID)[field.Key]; got != "Shared with my teams" {
		t.Error("archiving one team hid a field still shared through another active team")
	}
	if err := e.identity.RemoveTeamMember(t.Context(), system, second, teammate.PersonID); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.entry(teammate, subject.PersonID, field.Key); ok {
		t.Error("removed teammate still sees team field through an archived team")
	}
	if _, err := e.identity.SetTeamMember(t.Context(), system, second, teammate.PersonID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	if got := e.view(teammate, subject.PersonID)[field.Key]; got != "Shared with my teams" {
		t.Error("restored active membership did not restore visibility")
	}
	if _, err := e.identity.ArchiveTeam(t.Context(), system, second); err != nil {
		t.Fatal(err)
	}
	if _, ok := e.entry(teammate, subject.PersonID, field.Key); ok {
		t.Error("archived memberships still grant team field visibility")
	}
	for _, viewer := range []profile.Viewer{subject, admin} {
		if got := e.view(viewer, subject.PersonID)[field.Key]; got != "Shared with my teams" {
			t.Error("self/admin visibility incorrectly depends on active team membership")
		}
	}
}
