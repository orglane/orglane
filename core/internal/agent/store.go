package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// Files of an instance directory (docs/specs/agent.md "Instance directory").
const (
	instanceFile = "instance.json"
	desiredFile  = "desired.json"
	currentFile  = "current.json"
	blockedFile  = "blocked.json"
	// configuredFile holds the render hash the configure hooks last
	// completed for.
	configuredFile = "configured.json"
	stagingDir     = ".staging"
	dataDir        = "data"
)

// maxApplyAttempts is how often a failed apply of a generation runs before
// the generation is blocked: the first attempt and at most three retries.
const maxApplyAttempts = 4

// instanceState is what an instance directory holds.
type instanceState struct {
	id      uuid.UUID
	desired *agentapi.Spec
	current *agentapi.Spec
	blocked blockedState
}

// blockedState is blocked.json: generations the agent never tries again
// (failed or rolled-back upgrades, applies that failed maxApplyAttempts
// times), and the failure count of the generation being applied.
type blockedState struct {
	Generations []blockedGeneration `json:"generations"`
	Failures    *failureCount       `json:"failures,omitempty"`
}

type blockedGeneration struct {
	Generation int64     `json:"generation"`
	ErrorCode  string    `json:"error_code"`
	At         time.Time `json:"at"`
}

type failureCount struct {
	Generation int64 `json:"generation"`
	Count      int   `json:"count"`
}

func (b blockedState) has(gen int64) bool {
	return slices.ContainsFunc(b.Generations, func(g blockedGeneration) bool { return g.Generation == gen })
}

// code returns the error code a generation was blocked with.
func (b blockedState) code(gen int64) (string, bool) {
	i := slices.IndexFunc(b.Generations, func(g blockedGeneration) bool { return g.Generation == gen })
	if i < 0 {
		return "", false
	}
	return b.Generations[i].ErrorCode, true
}

func (b blockedState) list() []int64 {
	out := make([]int64, 0, len(b.Generations))
	for _, g := range b.Generations {
		out = append(out, g.Generation)
	}
	return out
}

// pending reports whether the desired spec still has to be applied.
func (s *instanceState) pending() bool {
	return s.desired != nil && (s.current == nil || s.desired.Generation > s.current.Generation) && !s.blocked.has(s.desired.Generation)
}

// purgedRecord is instances/.agent/purged/<key>.json: the last completed
// purge of a key. It lives outside the deleted directory, so a retry of the
// purge spec gets the operation that purged it, and a restart after the
// deletion reports that operation as succeeded.
type purgedRecord struct {
	InstanceID uuid.UUID `json:"instance_id"`
	Generation int64     `json:"generation"`
	SpecHash   string    `json:"spec_hash"`
	Operation  uuid.UUID `json:"operation_id"`
	At         time.Time `json:"at"`
}

// readPurged returns the purge record of a key, nil if there is none.
func readPurged(path string) (*purgedRecord, error) {
	var p purgedRecord
	ok, err := readJSON(path, &p)
	if !ok || err != nil {
		return nil, err
	}
	return &p, nil
}

// readBinding returns the instance ID a directory is bound to, uuid.Nil if
// it has none.
func readBinding(dir string) (uuid.UUID, error) {
	var bind struct {
		InstanceID uuid.UUID `json:"instance_id"`
	}
	if _, err := readJSON(filepath.Join(dir, instanceFile), &bind); err != nil {
		return uuid.Nil, err
	}
	return bind.InstanceID, nil
}

// loadState reads an instance directory. A missing directory is an empty
// state.
func loadState(dir string) (*instanceState, error) {
	st := &instanceState{}
	var err error
	if st.id, err = readBinding(dir); err != nil {
		return nil, err
	}
	if st.desired, err = readSpec(filepath.Join(dir, desiredFile)); err != nil {
		return nil, err
	}
	if st.current, err = readSpec(filepath.Join(dir, currentFile)); err != nil {
		return nil, err
	}
	if _, err := readJSON(filepath.Join(dir, blockedFile), &st.blocked); err != nil {
		return nil, err
	}
	return st, nil
}

func readSpec(path string) (*agentapi.Spec, error) {
	data, err := os.ReadFile(path) //nolint:gosec // paths are built from the data directory and validated keys
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s, err := decodeSpec(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// decodeSpec decodes a spec strictly; numbers stay json.Number, so a spec
// read back from disk hashes like the one received.
func decodeSpec(data []byte) (*agentapi.Spec, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	dec.UseNumber()
	var s agentapi.Spec
	if err := dec.Decode(&s); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after the spec")
	}
	return &s, nil
}

func readJSON(path string, v any) (bool, error) {
	data, err := os.ReadFile(path) //nolint:gosec // paths are built from the data directory and validated keys
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

// writeJSON writes an agent state file (mode 0600: specs hold secrets).
func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

// writeFileAtomic writes a file through a temporary file and a rename, and
// syncs both, so readers and crashes see the old or the new content.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = os.Remove(f.Name())
		}
	}()
	if err := f.Chmod(perm); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir) //nolint:gosec // agent-owned directory
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}
