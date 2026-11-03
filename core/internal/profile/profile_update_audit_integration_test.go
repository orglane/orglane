//go:build integration

package profile_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/profile"
	"github.com/orglane/orglane/tests/auditfault"
)

// A clear still reads the old value to compare the change; that read must
// commit its own audit even though the response no longer contains a value.
func TestSensitiveProfileClearAuditsExistingValue(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	operator := e.person("operator")
	operator.ManagePeople, operator.ReadSensitive = true, true
	field := e.library("emergency_contact")
	e.set(operator, subject.PersonID, field.Key, "Previous contact")
	before := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID)
	entries, err := e.profile.UpdateProfile(t.Context(), actorOf(operator), operator, subject.PersonID, []profile.Change{{Key: field.Key, Value: new("")}})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Field.ID == field.ID && entry.Value != "" {
			t.Error("clear response contains the removed sensitive value")
		}
	}
	if got := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); got != before+1 {
		t.Errorf("clear recorded %d sensitive reads, want 1", got-before)
	}
	var rows int
	if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM profile_field_values WHERE person_id = $1 AND field_id = $2`, subject.PersonID, field.ID).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Errorf("clear left %d stored values", rows)
	}
	metadata := e.lastAuditMetadata(audit.PersonSensitiveFieldsRead, subject.PersonID)
	if !slices.Equal(anyStrings(metadata["fields"]), []string{"emergency_contact"}) || metadata["@actor"] != operator.PersonID.String() {
		t.Errorf("clear read audit metadata: %v", metadata)
	}
}

// Corrupt ciphertext distinguishes validation before any decryption from a
// rejected edit that silently reads the old plaintext before returning.
func TestRejectedProfileChangesDoNotDecryptValues(t *testing.T) {
	for _, name := range []string{"invalid replacement", "invalid later field", "forbidden edit", "forbidden hide"} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t)
			subject := e.person("subject")
			subject.ReadSensitive = true
			operator := e.person("operator")
			operator.ManagePeople, operator.ReadSensitive = true, true
			field := e.field(profile.NewField{Label: "Private note", Type: profile.TypeLongText, Sensitive: true, EditableBy: profile.EditableByAdmins})
			ordinary := e.field(profile.NewField{Label: "Desk", Type: profile.TypeText})
			e.set(operator, subject.PersonID, field.Key, "Previous note", ordinary.Key, "A-1")
			if _, err := e.pool.Exec(t.Context(), `UPDATE profile_field_values SET value_sealed = $1 WHERE person_id = $2 AND field_id = $3`, []byte{1}, subject.PersonID, field.ID); err != nil {
				t.Fatal(err)
			}
			viewer := operator
			changes := []profile.Change{{Key: field.Key, Value: new("Replacement")}}
			wantErr := profile.ErrInvalid
			switch name {
			case "invalid replacement":
				changes[0].Value = new("invalid\x00value")
			case "invalid later field":
				changes = append(changes, profile.Change{Key: ordinary.Key, Value: new("two\nlines")})
			case "forbidden edit":
				viewer = subject
				wantErr = profile.ErrForbidden
			case "forbidden hide":
				changes = []profile.Change{{Key: field.Key, Hidden: new(true)}}
				wantErr = profile.ErrForbidden
			}
			before := auditfault.Snapshot(t, e.pool)
			entries, err := e.profile.UpdateProfile(t.Context(), actorOf(viewer), viewer, subject.PersonID, changes)
			if !errors.Is(err, wantErr) || entries != nil {
				t.Errorf("rejected change returned entries=%v err=%v, want %v without decrypting", entries, err, wantErr)
			}
			if changed := before.Changed(auditfault.Snapshot(t, e.pool)); len(changed) != 0 {
				t.Errorf("rejected change modified tables %v", changed)
			}
		})
	}
}

func TestSensitiveProfileUpdateReadAuditFailureRollsBackBatch(t *testing.T) {
	for _, tc := range []struct {
		name              string
		existing, replace bool
		failAt, reads     int
	}{
		{"existing value read", true, true, 1, 2},
		{"later response read", true, true, 2, 2},
		{"unrelated response value", true, false, 1, 1},
		{"new response value", false, true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			subject := e.person("subject")
			operator := e.person("operator")
			operator.ManagePeople, operator.ReadSensitive = true, true
			field := e.library("emergency_contact")
			ordinary := e.field(profile.NewField{Label: "Desk", Type: profile.TypeText})
			e.set(operator, subject.PersonID, ordinary.Key, "A-1")
			if tc.existing {
				e.set(operator, subject.PersonID, field.Key, "Previous contact")
			}
			changes := []profile.Change{{Key: ordinary.Key, Value: new("B-2")}}
			if tc.replace {
				changes = append(changes, profile.Change{Key: field.Key, Value: new("Replacement contact")})
			}
			beforeReads := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID)
			beforeChanges := e.auditCount(audit.PersonProfileUpdated, subject.PersonID)
			fault := auditfault.Inject(t, e.pool, "NEW.action = '"+string(audit.PersonSensitiveFieldsRead)+"'", tc.failAt)
			before := auditfault.Snapshot(t, e.pool)
			entries, err := e.profile.UpdateProfile(t.Context(), actorOf(operator), operator, subject.PersonID, changes)
			if !auditfault.Is(err) || entries != nil {
				t.Errorf("rejected read audit returned entries=%v err=%v", entries, err)
			}
			if changed := before.Changed(auditfault.Snapshot(t, e.pool)); len(changed) != 0 {
				t.Errorf("failed batch persisted changes in %v", changed)
			}
			if hits := fault.Hits(); hits != 1 {
				t.Errorf("expected audit failure reached %d times, want 1", hits)
			}
			fault.Remove()
			entries, err = e.profile.UpdateProfile(t.Context(), actorOf(operator), operator, subject.PersonID, changes)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Field.ID == ordinary.ID && entry.Value != "B-2" {
					t.Error("successful batch did not update ordinary value")
				}
				if tc.replace && entry.Field.ID == field.ID && entry.Value != "Replacement contact" {
					t.Error("successful batch did not update sensitive value")
				}
			}
			if got := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); got != beforeReads+tc.reads {
				t.Errorf("successful batch recorded %d reads, want %d", got-beforeReads, tc.reads)
			}
			if got := e.auditCount(audit.PersonProfileUpdated, subject.PersonID); got != beforeChanges+1 {
				t.Errorf("successful batch recorded %d mutations, want 1", got-beforeChanges)
			}
		})
	}
}

// A failed read audit must also prevent clearing the old sensitive value.
func TestSensitiveProfileClearReadAuditFailureKeepsValue(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	operator := e.person("operator")
	operator.ManagePeople, operator.ReadSensitive = true, true
	field := e.library("emergency_contact")
	e.set(operator, subject.PersonID, field.Key, "Previous contact")
	var ciphertext []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT value_sealed FROM profile_field_values WHERE person_id = $1 AND field_id = $2`, subject.PersonID, field.ID).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	fault := auditfault.Inject(t, e.pool, "NEW.action = '"+string(audit.PersonSensitiveFieldsRead)+"'", 1)
	entries, err := e.profile.UpdateProfile(t.Context(), actorOf(operator), operator, subject.PersonID, []profile.Change{{Key: field.Key, Value: new("")}})
	if !auditfault.Is(err) || entries != nil {
		t.Errorf("clear without committed read audit returned entries=%v err=%v", entries, err)
	}
	var retained []byte
	if err := e.pool.QueryRow(t.Context(), `SELECT value_sealed FROM profile_field_values WHERE person_id = $1 AND field_id = $2`, subject.PersonID, field.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(ciphertext, retained) {
		t.Error("failed clear changed the sensitive ciphertext")
	}
	if fault.Hits() != 1 {
		t.Error("clear did not reach its read audit")
	}
}
