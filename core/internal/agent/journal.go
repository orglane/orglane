package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// journalFile is the upgrade journal, present only while an upgrade or a
// restore runs (docs/specs/agent.md "Upgrade").
const journalFile = "upgrade.json"

// Decisions recorded in the journal while an upgrade awaits confirmation.
const (
	decisionAbort = "abort"
)

// journal is upgrade.json: written and synced before each phase starts, so
// a restarted agent knows where an upgrade or a restore stopped. It holds
// the target spec (secrets) and is mode 0600 like the other state files.
type journal struct {
	Operation uuid.UUID `json:"operation"`
	// Kind is agentapi.KindUpgrade or agentapi.KindRestore.
	Kind           string               `json:"kind"`
	From           *agentapi.PackageRef `json:"from,omitempty"` // the version that ran before an upgrade
	To             agentapi.PackageRef  `json:"to"`
	FromGeneration int64                `json:"from_generation,omitempty"`
	ToGeneration   int64                `json:"to_generation"`
	SnapshotID     uuid.UUID            `json:"snapshot_id"`
	Phase          string               `json:"phase"`
	// ErrorCode is why an upgrade rolls back.
	ErrorCode string `json:"error_code,omitempty"`
	// ConfirmBy is the confirmation deadline in awaiting_confirmation.
	ConfirmBy *time.Time `json:"confirm_by,omitempty"`
	// Decision is decisionAbort once an abort or the confirmation timeout
	// decided to roll back; a confirmation moves the phase to committing.
	Decision string `json:"decision,omitempty"`
	// Routed are the names a route of the instance may target during the
	// operation (fence.go): those of the version that ran when it started
	// and of the upgrade's target. RoutedKnown is set once the running
	// deployment's names are in it (preflight of an upgrade, the first run
	// of a restore).
	Routed      []string `json:"routed,omitempty"`
	RoutedKnown bool     `json:"routed_known,omitempty"`
	// RollbackFailed: the fenced restore failed. Nothing is retried until a
	// restore request (POST /v1/instances/{key}/restore).
	RollbackFailed bool `json:"rollback_failed,omitempty"`
	// Spec is the upgrade's target spec, or the spec a restore restores.
	Spec *agentapi.Spec `json:"spec"`
}

// active reports whether the journal blocks other work on the instance: a
// running upgrade or restore, or one to recover. A failed rollback does
// not block a restore.
func (j *journal) active() bool { return j != nil && !j.RollbackFailed }

func readJournal(dir string) (*journal, error) {
	data, err := os.ReadFile(filepath.Join(dir, journalFile)) //nolint:gosec // agent-owned path
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Numbers stay json.Number, so the spec hashes like the one received.
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var j journal
	if err := dec.Decode(&j); err != nil {
		return nil, fmt.Errorf("%s: %w", journalFile, err)
	}
	if j.Spec == nil {
		return nil, fmt.Errorf("%s: no spec", journalFile)
	}
	return &j, nil
}

func writeJournal(dir string, j *journal) error { return writeJSON(filepath.Join(dir, journalFile), j) }

func removeJournal(dir string) error {
	if err := os.Remove(filepath.Join(dir, journalFile)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}
