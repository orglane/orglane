package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// decodeStrict decodes a JSON body, refusing unknown fields and trailing
// data.
func decodeStrict(body []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}

// busy reports whether an operation runs or waits on the instance, which a
// snapshot or restore must not overlap (409 agent_busy). A reconcile
// without an operation (drift repair) is not one: the request runs after
// it.
func (a *Agent) busy(in *instance, j *journal) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.busyLocked(in, j)
}

func (a *Agent) busyLocked(in *instance, j *journal) bool {
	return (in.busy && in.busyKind != "") || in.snapshotOp != uuid.Nil || j.active()
}

// handleSnapshot answers POST /v1/instances/{key}/snapshots: a manual
// snapshot, taken by the instance's worker.
func (a *Agent) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	key, ok := pathKey(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body) // read and bounded by authenticate
	var req agentapi.SnapshotRequest
	if err := decodeStrict(body, &req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, agentapi.CodeInvalidInput, "malformed snapshot request", nil)
		return
	}
	if req.Reason != agentapi.SnapshotReasonManual {
		writeProblem(w, r, http.StatusUnprocessableEntity, agentapi.CodeInvalidInput, "the reason of a requested snapshot is manual",
			func(p *agentapi.Problem) { p.Fields = []string{"reason"} })
		return
	}
	in := a.instance(key)
	in.files.Lock()
	st, err := loadState(a.instanceDir(key))
	var j *journal
	if err == nil {
		j, err = readJournal(a.instanceDir(key))
	}
	in.files.Unlock()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if st.id == uuid.Nil || st.current == nil || st.current.State == agentapi.StateAbsent {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "the instance is not installed", nil)
		return
	}
	a.mu.Lock()
	if a.busyLocked(in, j) {
		a.mu.Unlock()
		writeProblem(w, r, http.StatusConflict, agentapi.CodeAgentBusy, "an operation runs on the instance", nil)
		return
	}
	op, err := a.ops.create(key, agentapi.KindSnapshot, 0)
	if err == nil {
		in.snapshotOp = op.ID
	}
	a.mu.Unlock()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	a.log.Info("snapshot requested", "instance", key, "operation", op.ID)
	a.wake(key)
	writeJSONResponse(w, http.StatusAccepted, op)
}

// handleSnapshots answers GET /v1/instances/{key}/snapshots.
func (a *Agent) handleSnapshots(w http.ResponseWriter, r *http.Request) {
	key, ok := pathKey(w, r)
	if !ok {
		return
	}
	in := a.instance(key)
	in.files.Lock()
	st, err := loadState(a.instanceDir(key))
	var list []agentapi.Snapshot
	if err == nil && st.id != uuid.Nil {
		list, err = a.listSnapshots(key)
	}
	in.files.Unlock()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if st.id == uuid.Nil {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "unknown instance", nil)
		return
	}
	writeJSONResponse(w, http.StatusOK, agentapi.SnapshotList{Snapshots: list})
}

// handleRestore answers POST /v1/instances/{key}/restore: the snapshot's
// spec becomes the accepted spec with the requested generation, and the
// restore runs the two journaled rollback phases (docs/specs/agent.md
// "Upgrade"). It also retries a failed rollback.
func (a *Agent) handleRestore(w http.ResponseWriter, r *http.Request) {
	key, ok := pathKey(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req agentapi.RestoreRequest
	if err := decodeStrict(body, &req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, agentapi.CodeInvalidInput, "malformed restore request", nil)
		return
	}
	var fields []string
	if req.SnapshotID == uuid.Nil {
		fields = append(fields, "snapshot_id")
	}
	if req.Generation <= 0 {
		fields = append(fields, "generation")
	}
	if len(fields) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, agentapi.CodeInvalidInput, "invalid restore request",
			func(p *agentapi.Problem) { p.Fields = fields })
		return
	}
	in := a.instance(key)
	dir := a.instanceDir(key)
	in.files.Lock()
	op, status, problem := a.acceptRestore(in, dir, req)
	in.files.Unlock()
	if problem != nil {
		problem(w, r)
		return
	}
	a.log.Info("restore requested", "instance", key, "snapshot", req.SnapshotID, "generation", req.Generation, "operation", op.ID)
	a.wake(key)
	writeJSONResponse(w, status, op)
}

func (a *Agent) acceptRestore(in *instance, dir string, req agentapi.RestoreRequest) (agentapi.Operation, int, func(http.ResponseWriter, *http.Request)) {
	fail := func(status int, code, detail string, extra func(*agentapi.Problem)) (agentapi.Operation, int, func(http.ResponseWriter, *http.Request)) {
		return agentapi.Operation{}, 0, func(w http.ResponseWriter, r *http.Request) { writeProblem(w, r, status, code, detail, extra) }
	}
	internal := func(err error) (agentapi.Operation, int, func(http.ResponseWriter, *http.Request)) {
		return agentapi.Operation{}, 0, func(w http.ResponseWriter, r *http.Request) { a.internalError(w, r, err) }
	}
	st, err := loadState(dir)
	if err != nil {
		return internal(err)
	}
	j, err := readJournal(dir)
	if err != nil {
		return internal(err)
	}
	if st.id == uuid.Nil {
		return fail(http.StatusNotFound, agentapi.CodeNotFound, "unknown instance", nil)
	}
	// Paths are built from the snapshot directory's own name, never from
	// the request.
	snapID, ok := a.findSnapshot(in.key, req.SnapshotID)
	if !ok {
		return fail(http.StatusNotFound, agentapi.CodeNotFound, "unknown or incomplete snapshot", nil)
	}
	_, spec, err := a.loadSnapshot(in.key, snapID)
	if err != nil {
		return fail(http.StatusNotFound, agentapi.CodeNotFound, "unknown or incomplete snapshot", nil)
	}
	if a.busy(in, j) {
		return fail(http.StatusConflict, agentapi.CodeAgentBusy, "an operation runs on the instance", nil)
	}
	if spec.InstanceID != st.id {
		return fail(http.StatusConflict, agentapi.CodeInstanceMismatch, "the snapshot belongs to another instance", nil)
	}
	if st.desired != nil && req.Generation <= st.desired.Generation {
		accepted := st.desired.Generation
		return fail(http.StatusConflict, agentapi.CodeStaleGeneration, "the generation must be newer than the accepted one",
			func(p *agentapi.Problem) { p.AcceptedGeneration = accepted })
	}
	restored := *spec
	restored.Generation = req.Generation
	if err := a.supersede(in.key, req.Generation); err != nil {
		return internal(err)
	}
	// The operation, then the journal, then the accepted spec: a crash in
	// between leaves an interrupted operation and nothing changed, or a
	// journal that recovery finishes (finalize also records the spec as
	// accepted).
	op, err := a.ops.create(in.key, agentapi.KindRestore, req.Generation)
	if err != nil {
		return internal(err)
	}
	if _, err := a.ops.update(op.ID, func(o *agentapi.Operation) { o.SnapshotID = snapID.String() }); err != nil {
		return internal(err)
	}
	nj := &journal{
		Operation: op.ID, Kind: agentapi.KindRestore, To: spec.Package, ToGeneration: req.Generation,
		SnapshotID: snapID, Phase: agentapi.PhaseRollingBack, Spec: &restored,
	}
	if j != nil {
		// A failed rollback: its routes may still be active.
		nj.Routed = j.Routed
	}
	if err := writeJournal(dir, nj); err != nil {
		return internal(err)
	}
	if err := writeSpec(filepath.Join(dir, desiredFile), &restored); err != nil {
		return internal(err)
	}
	op, _ = a.ops.get(op.ID)
	return op, http.StatusAccepted, nil
}

// handleDecision answers POST /v1/operations/{id}/confirm and /abort.
func (a *Agent) handleDecision(w http.ResponseWriter, r *http.Request, confirm bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "unknown operation", nil)
		return
	}
	op, code := a.decide(id, confirm)
	switch code {
	case "":
		writeJSONResponse(w, http.StatusOK, op)
	case agentapi.CodeNotFound:
		writeProblem(w, r, http.StatusNotFound, code, "unknown operation", nil)
	case agentapi.CodeNotAwaitingConfirmation:
		writeProblem(w, r, http.StatusConflict, code, "the operation is not an upgrade awaiting its confirmation", nil)
	default:
		writeProblem(w, r, http.StatusInternalServerError, agentapi.CodeInternal, "", nil)
	}
}
