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

// opStore keeps operations in memory and persists each one under
// instances/.agent/operations/<id>.json.
type opStore struct {
	dir          string
	instancesDir string
	now          func() time.Time

	mu  sync.Mutex
	ops map[uuid.UUID]*agentapi.Operation
}

func newOpStore(dir, instancesDir string, now func() time.Time) *opStore {
	return &opStore{dir: dir, instancesDir: instancesDir, now: now, ops: map[uuid.UUID]*agentapi.Operation{}}
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
		var op agentapi.Operation
		if _, err := readJSON(filepath.Join(s.dir, name), &op); err != nil {
			return err
		}
		if op.ID.String()+".json" != name {
			return fmt.Errorf("operation file %s holds operation %s", name, op.ID)
		}
		s.ops[op.ID] = &op
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
		}
	}
	return errors.Join(errs...)
}

func (s *opStore) path(id uuid.UUID) string { return filepath.Join(s.dir, id.String()+".json") }

// create persists a new queued operation.
func (s *opStore) create(instance, kind string, generation int64) (agentapi.Operation, error) {
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
	if err := writeJSON(s.path(id), op); err != nil {
		return agentapi.Operation{}, err
	}
	s.ops[id] = op
	return *op, nil
}

// adopt re-creates an operation that a journal names but the store does
// not know (its file was lost), as running, so the journal's result can
// still be recorded. A known operation is left as it is.
func (s *opStore) adopt(id uuid.UUID, instance, kind, phase string, generation int64) error {
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
	if err := writeJSON(s.path(id), op); err != nil {
		return err
	}
	s.ops[id] = op
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
	if err := writeJSON(s.path(id), &next); err != nil {
		return *op, err
	}
	*op = next
	return next, nil
}

// find returns the newest operation of an instance that matches.
func (s *opStore) find(instance string, match func(*agentapi.Operation) bool) (agentapi.Operation, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
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
// generation.
func (s *opStore) forGeneration(instance string, gen int64) (agentapi.Operation, bool) {
	return s.find(instance, func(op *agentapi.Operation) bool {
		return op.Generation == gen && (op.Kind == agentapi.KindApply || op.Kind == agentapi.KindRemove || op.Kind == agentapi.KindUpgrade)
	})
}

// latest returns the newest operation of an instance.
func (s *opStore) latest(instance string) (agentapi.Operation, bool) {
	return s.find(instance, func(*agentapi.Operation) bool { return true })
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
