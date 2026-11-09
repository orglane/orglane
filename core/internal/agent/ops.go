package agent

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// operationRetention is how long finished operations are kept, so the core
// can poll them across agent restarts.
const operationRetention = 7 * 24 * time.Hour

// operationFile is the persisted form of an operation: the wire operation
// plus the immutable instance it belongs to. The owner is agent state, not
// part of the API: a key reused after a purge must not return, recover or
// supersede the previous instance's operations. Operations written by an
// agent version that did not record the owner have no instance_id; startup
// assigns them to the instance that owns them (assignLegacyOwners).
type operationFile struct {
	agentapi.Operation
	InstanceID uuid.UUID `json:"instance_id,omitempty"`
}

// opStore keeps operations in memory and persists each one under
// instances/.agent/operations/<id>.json.
type opStore struct {
	dir          string
	instancesDir string
	now          func() time.Time
	// observed, if set, is called with every updated operation. Tests use
	// it to assert invariants of published states.
	observed func(agentapi.Operation)

	mu     sync.Mutex
	ops    map[uuid.UUID]*agentapi.Operation
	owners map[uuid.UUID]uuid.UUID
}

func newOpStore(dir, instancesDir string, now func() time.Time) *opStore {
	return &opStore{
		dir: dir, instancesDir: instancesDir, now: now,
		ops: map[uuid.UUID]*agentapi.Operation{}, owners: map[uuid.UUID]uuid.UUID{},
	}
}

// load reads the persisted operations and deletes those finished more than
// operationRetention ago, unless a journal still references them.
func (s *opStore) load() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || strings.HasPrefix(name, ".") {
			continue
		}
		var f operationFile
		if _, err := readJSON(filepath.Join(s.dir, name), &f); err != nil {
			return err
		}
		op := f.Operation
		if op.ID.String()+".json" != name {
			return fmt.Errorf("operation file %s holds operation %s", name, op.ID)
		}
		s.ops[op.ID] = &op
		s.owners[op.ID] = f.InstanceID
	}
	return s.pruneLocked()
}

func (s *opStore) prune() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pruneLocked()
}

func (s *opStore) pruneLocked() error {
	cutoff := s.now().Add(-operationRetention)
	var errs []error
	for id, op := range s.ops {
		if op.FinishedAt != nil && op.FinishedAt.Before(cutoff) {
			// A crash may leave a terminal result with a journal whose
			// deletion still needs retry: finalize records the result
			// again, so the operation stays while its journal exists.
			// Journals are replaced atomically, so reading one needs no
			// instance file lock (which would invert the lock order).
			if agentapi.KeyPattern.MatchString(op.Instance) {
				j, err := readJournal(filepath.Join(s.instancesDir, op.Instance))
				if err != nil {
					errs = append(errs, fmt.Errorf("retain operation %s: %w", id, err))
					continue
				}
				if j != nil && j.Operation == id {
					continue
				}
			}
			if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				errs = append(errs, err)
				continue
			}
			delete(s.ops, id)
			delete(s.owners, id)
		}
	}
	return errors.Join(errs...)
}

func (s *opStore) path(id uuid.UUID) string { return filepath.Join(s.dir, id.String()+".json") }

// writeLocked persists an operation with its owner. The caller holds mu.
func (s *opStore) writeLocked(op *agentapi.Operation, owner uuid.UUID) error {
	return writeJSON(s.path(op.ID), operationFile{Operation: *op, InstanceID: owner})
}

// create persists a new queued operation owned by one instance.
func (s *opStore) create(instance, kind string, generation int64, owner uuid.UUID) (agentapi.Operation, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return agentapi.Operation{}, err
	}
	op := &agentapi.Operation{
		ID: id, Instance: instance, Kind: kind, Generation: generation,
		State: agentapi.OpQueued, StartedAt: s.now().UTC(),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.writeLocked(op, owner); err != nil {
		return agentapi.Operation{}, err
	}
	s.ops[id] = op
	s.owners[id] = owner
	return *op, nil
}

// recreate records an operation again under the ID of a pruned operation,
// as running, so a retry reports the same identity and terminal result.
func (s *opStore) recreate(id uuid.UUID, instance, kind string, generation int64, owner uuid.UUID) (agentapi.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.ops[id]; ok {
		return *op, nil
	}
	sec, nsec := id.Time().UnixTime()
	op := &agentapi.Operation{
		ID: id, Instance: instance, Kind: kind, Generation: generation,
		State: agentapi.OpRunning, StartedAt: time.Unix(sec, nsec).UTC(),
	}
	if err := s.writeLocked(op, owner); err != nil {
		return agentapi.Operation{}, err
	}
	s.ops[id] = op
	s.owners[id] = owner
	return *op, nil
}

// adopt re-creates an operation that a journal names but the store does
// not know (its file was lost), as running, so the journal's result can
// still be recorded. A known operation is left as it is.
func (s *opStore) adopt(id uuid.UUID, instance, kind, phase string, generation int64, owner uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[id]; ok {
		return nil
	}
	sec, nsec := id.Time().UnixTime()
	op := &agentapi.Operation{
		ID: id, Instance: instance, Kind: kind, Generation: generation,
		State: agentapi.OpRunning, Phase: phase, StartedAt: time.Unix(sec, nsec).UTC(),
	}
	if err := s.writeLocked(op, owner); err != nil {
		return err
	}
	s.ops[id] = op
	s.owners[id] = owner
	return nil
}

func (s *opStore) get(id uuid.UUID) (agentapi.Operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return agentapi.Operation{}, false
	}
	return *op, true
}

// update changes an operation and persists it. Terminal operations get
// their finish time.
func (s *opStore) update(id uuid.UUID, fn func(*agentapi.Operation)) (agentapi.Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.ops[id]
	if !ok {
		return agentapi.Operation{}, fmt.Errorf("operation %s not found", id)
	}
	next := *op
	fn(&next)
	if next.Terminal() && next.FinishedAt == nil {
		t := s.now().UTC()
		next.FinishedAt = &t
	}
	if err := s.writeLocked(&next, s.owners[id]); err != nil {
		return *op, err
	}
	*op = next
	if s.observed != nil {
		s.observed(next)
	}
	return next, nil
}

// findLocked returns the newest operation of an instance that matches. The
// caller holds mu.
func (s *opStore) findLocked(instance string, match func(*agentapi.Operation) bool) (agentapi.Operation, bool) {
	var best *agentapi.Operation
	for _, op := range s.ops {
		if op.Instance != instance || !match(op) {
			continue
		}
		// UUIDv7 IDs sort by creation time.
		if best == nil || op.ID.String() > best.ID.String() {
			best = op
		}
	}
	if best == nil {
		return agentapi.Operation{}, false
	}
	return *best, true
}

// forGeneration returns the newest apply, remove or upgrade operation of a
// generation that belongs to one instance.
func (s *opStore) forGeneration(instance string, gen int64, owner uuid.UUID) (agentapi.Operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(instance, func(op *agentapi.Operation) bool {
		return s.owners[op.ID] == owner && op.Generation == gen &&
			(op.Kind == agentapi.KindApply || op.Kind == agentapi.KindRemove || op.Kind == agentapi.KindUpgrade)
	})
}

// latest returns the newest operation of an instance that belongs to it.
func (s *opStore) latest(instance string, owner uuid.UUID) (agentapi.Operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findLocked(instance, func(op *agentapi.Operation) bool { return s.owners[op.ID] == owner })
}

// unfinished returns the operations that are not terminal.
func (s *opStore) unfinished() []agentapi.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agentapi.Operation
	for _, op := range s.ops {
		if !op.Terminal() {
			out = append(out, *op)
		}
	}
	return out
}

// unfinishedOwned returns the unfinished operations of an instance that
// belong to it.
func (s *opStore) unfinishedOwned(instance string, owner uuid.UUID) []agentapi.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agentapi.Operation
	for _, op := range s.ops {
		if !op.Terminal() && op.Instance == instance && s.owners[op.ID] == owner {
			out = append(out, *op)
		}
	}
	return out
}

// ownerOf returns the instance an operation belongs to, uuid.Nil if its file
// predates recorded ownership.
func (s *opStore) ownerOf(id uuid.UUID) uuid.UUID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.owners[id]
}

// setOwner assigns an operation persisted without an owner to its instance.
func (s *opStore) setOwner(id, owner uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.ops[id]; ok {
		s.owners[id] = owner
	}
}

// unowned returns the operations without an owner, to assign them at start.
func (s *opStore) unowned() []agentapi.Operation {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []agentapi.Operation
	for id, op := range s.ops {
		if s.owners[id] == uuid.Nil {
			out = append(out, *op)
		}
	}
	return out
}
