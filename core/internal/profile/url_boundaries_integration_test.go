//go:build integration

package profile_test

import (
	"bytes"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/profile"
)

func TestURLStorageBoundariesAndDeclassification(t *testing.T) {
	const prefix = "https://example.com/"
	for _, sensitive := range []bool{false, true} {
		for _, length := range []int{2000, 2001, 2048} {
			t.Run(strconv.FormatBool(sensitive)+"/"+strconv.Itoa(length), func(t *testing.T) {
				e := newEnv(t)
				subject := e.person("subject")
				operator := e.person("operator")
				operator.ManagePeople, operator.ReadSensitive = true, true
				field := e.field(profile.NewField{Label: "Website", Type: profile.TypeURL, Sensitive: sensitive})
				value := prefix + strings.Repeat("a", length-len(prefix))
				entries, err := e.profile.UpdateProfile(t.Context(), actorOf(operator), operator, subject.PersonID, []profile.Change{{Key: field.Key, Value: &value}})
				if length > 2000 {
					if !errors.Is(err, profile.ErrInvalid) || entries != nil {
						t.Errorf("over-limit URL returned entries=%v err=%v, want validation error", entries, err)
					}
					var count int
					if err := e.pool.QueryRow(t.Context(), `SELECT count(*) FROM profile_field_values WHERE person_id = $1 AND field_id = $2`, subject.PersonID, field.ID).Scan(&count); err != nil {
						t.Fatal(err)
					}
					if count != 0 {
						t.Errorf("rejected URL persisted %d values", count)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if sensitive {
					if _, err := e.profile.UpdateField(t.Context(), actorOf(operator), operator, field.ID, profile.FieldUpdate{Sensitive: new(false)}); err != nil {
						t.Fatalf("declassification: %v", err)
					}
				}
				if length == 2000 {
					var plaintext string
					if err := e.pool.QueryRow(t.Context(), `SELECT value FROM profile_field_values WHERE person_id = $1 AND field_id = $2`, subject.PersonID, field.ID).Scan(&plaintext); err != nil {
						t.Fatal(err)
					}
					if plaintext != value {
						t.Error("valid boundary URL did not survive plaintext storage/declassification")
					}
				}
			})
		}
	}
}

func TestUnicodeURLCharacterLimitMatchesStorage(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	field := e.field(profile.NewField{Label: "Website", Type: profile.TypeURL})
	const prefix = "https://example.com/"
	value := prefix + strings.Repeat("界", 2000-len(prefix))
	e.set(subject, subject.PersonID, field.Key, value)
	var characters int
	if err := e.pool.QueryRow(t.Context(), `SELECT length(value) FROM profile_field_values WHERE person_id = $1 AND field_id = $2`, subject.PersonID, field.ID).Scan(&characters); err != nil {
		t.Fatal(err)
	}
	if characters != 2000 {
		t.Errorf("stored character length %d, want 2000", characters)
	}
}

// Older unreleased versions accepted longer encrypted URLs. Declassifying
// such a value must be a validation error with its read audited, not a DB 500.
func TestOversizedEncryptedURLCannotBeDeclassified(t *testing.T) {
	e := newEnv(t)
	subject := e.person("subject")
	operator := e.person("operator")
	operator.ManagePeople, operator.ReadSensitive = true, true
	field := e.field(profile.NewField{Label: "Website", Type: profile.TypeURL, Sensitive: true})
	const prefix = "https://example.com/"
	value := prefix + strings.Repeat("a", 2001-len(prefix))
	ciphertext := e.keyring.Sealer(crypto.PurposeProfileFields).Seal([]byte(value), rowAD(subject.PersonID, field.ID))
	if _, err := e.pool.Exec(t.Context(), `INSERT INTO profile_field_values (person_id, field_id, value_sealed) VALUES ($1, $2, $3)`, subject.PersonID, field.ID, ciphertext); err != nil {
		t.Fatal(err)
	}
	before := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID)
	if _, err := e.profile.UpdateField(t.Context(), actorOf(operator), operator, field.ID, profile.FieldUpdate{Sensitive: new(false)}); !errors.Is(err, profile.ErrInvalid) {
		t.Errorf("oversized URL declassification: %v, want ErrInvalid", err)
	}
	var retained []byte
	var sensitive bool
	if err := e.pool.QueryRow(t.Context(), `SELECT f.sensitive, v.value_sealed FROM profile_fields f JOIN profile_field_values v ON v.field_id = f.id WHERE f.id = $1 AND v.person_id = $2`, field.ID, subject.PersonID).Scan(&sensitive, &retained); err != nil {
		t.Fatal(err)
	}
	if !sensitive || !bytes.Equal(ciphertext, retained) {
		t.Error("invalid declassification changed field sensitivity or ciphertext")
	}
	if got := e.auditCount(audit.PersonSensitiveFieldsRead, subject.PersonID); got != before+1 {
		t.Errorf("invalid declassification audited %d reads, want 1", got-before)
	}
}
