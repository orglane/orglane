package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
)

// Routes returns the agent's route table. It is exactly the declarative API
// of docs/specs/agent.md for this milestone; there is no route that takes a
// command, an image, a Compose file or a path. Snapshots, restores and
// upgrade confirmation follow with the upgrade flow (#60).
func Routes() []string {
	return []string{
		"GET " + agentapi.PathHealth,
		"GET " + agentapi.PathInstances,
		"GET " + agentapi.PathInstances + "/{key}",
		"PUT " + agentapi.PathInstances + "/{key}",
		"GET " + agentapi.PathInstances + "/{key}/logs",
		"GET /v1/operations/{id}",
	}
}

// Handler returns the agent's HTTP handler: every request is authenticated
// by its signature, every answer to an authenticated request is signed.
func (a *Agent) Handler() http.Handler {
	handlers := map[string]http.HandlerFunc{
		"GET " + agentapi.PathHealth:                    a.handleHealth,
		"GET " + agentapi.PathInstances:                 a.handleList,
		"GET " + agentapi.PathInstances + "/{key}":      a.handleGet,
		"PUT " + agentapi.PathInstances + "/{key}":      a.handlePut,
		"GET " + agentapi.PathInstances + "/{key}/logs": a.handleLogs,
		"GET /v1/operations/{id}":                       a.handleOperation,
	}
	mux := http.NewServeMux()
	for _, pattern := range Routes() {
		mux.HandleFunc(pattern, handlers[pattern])
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "no such endpoint", nil)
	})
	return a.authenticate(mux)
}

// bufferedResponse holds a response until it is signed.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(status int) {
	if b.status == 0 {
		b.status = status
	}
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}

// authenticate verifies request signatures (docs/specs/agent.md "Transport
// and authentication") and signs the responses to verified requests.
// Responses to requests that fail verification are not signed, so the agent
// is no signing oracle for unauthenticated clients.
func (a *Agent) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, agentapi.MaxRequestBytes+1))
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, agentapi.CodeInvalidInput, "unreadable request body", nil)
			return
		}
		if len(body) > agentapi.MaxRequestBytes {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, agentapi.CodeInvalidInput, "request body too large", nil)
			return
		}
		mac, err := a.verifier.Verify(r.Method, r.URL, r.Header, body)
		if err != nil {
			a.log.Warn("refused an unauthenticated request", "method", r.Method, "path", r.URL.Path, "reason", err)
			writeProblem(w, r, http.StatusUnauthorized, agentapi.CodeUnauthenticated, "", nil)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		rec := &bufferedResponse{header: http.Header{}}
		next.ServeHTTP(rec, r)
		if rec.status == 0 {
			rec.status = http.StatusOK
		}
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.Header().Set(agentapi.HeaderSignature, agentapi.SignResponse(a.key, mac, rec.status, rec.body.Bytes()))
		w.Header().Set("Content-Length", strconv.Itoa(rec.body.Len()))
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, code, detail string, extra func(*agentapi.Problem)) {
	p := agentapi.Problem{
		Type: "about:blank", Title: http.StatusText(status), Status: status,
		Detail: detail, Instance: r.URL.Path, Code: code,
	}
	if extra != nil {
		extra(&p)
	}
	body, _ := json.Marshal(p)
	w.Header().Set("Content-Type", agentapi.ProblemContentType)
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

func (a *Agent) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSONResponse(w, http.StatusOK, a.health())
}

func (a *Agent) handleList(w http.ResponseWriter, r *http.Request) {
	keys, err := a.keys()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	list := agentapi.InstanceList{Instances: []agentapi.InstanceStatus{}}
	for _, key := range keys {
		st, ok, err := a.status(r.Context(), key)
		if err != nil {
			a.internalError(w, r, err)
			return
		}
		if ok {
			list.Instances = append(list.Instances, st)
		}
	}
	writeJSONResponse(w, http.StatusOK, list)
}

// pathKey returns a validated instance key from the path, or answers 404.
func pathKey(w http.ResponseWriter, r *http.Request) (string, bool) {
	key := r.PathValue("key")
	if !agentapi.KeyPattern.MatchString(key) {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "invalid instance key", nil)
		return "", false
	}
	return key, true
}

func (a *Agent) handleGet(w http.ResponseWriter, r *http.Request) {
	key, ok := pathKey(w, r)
	if !ok {
		return
	}
	st, found, err := a.status(r.Context(), key)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if !found {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "unknown instance", nil)
		return
	}
	writeJSONResponse(w, http.StatusOK, st)
}

func (a *Agent) handleOperation(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	op, ok := a.ops.get(id)
	if err != nil || !ok {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "unknown operation", nil)
		return
	}
	writeJSONResponse(w, http.StatusOK, op)
}

func (a *Agent) internalError(w http.ResponseWriter, r *http.Request, err error) {
	a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
	writeProblem(w, r, http.StatusInternalServerError, agentapi.CodeInternal, "", nil)
}

// handlePut accepts a desired spec (docs/specs/agent.md "PUT
// /v1/instances/{key} semantics").
func (a *Agent) handlePut(w http.ResponseWriter, r *http.Request) {
	key, ok := pathKey(w, r)
	if !ok {
		return
	}
	body, _ := io.ReadAll(r.Body) // already read and bounded by authenticate
	spec, err := decodeSpec(body)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, agentapi.CodeInvalidInput, "malformed spec", nil)
		return
	}
	if fields := spec.Check(); len(fields) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, agentapi.CodeInvalidInput, "invalid spec", func(p *agentapi.Problem) { p.Fields = fields })
		return
	}
	pkg, ok := a.registry.Get(spec.Package)
	if !ok {
		writeProblem(w, r, http.StatusUnprocessableEntity, agentapi.CodeUnknownPackageVersion, "the package version is not embedded in this agent", nil)
		return
	}
	if _, err := pkg.Inputs(spec); err != nil {
		var ie *packages.InputError
		if !errors.As(err, &ie) {
			a.internalError(w, r, err)
			return
		}
		writeProblem(w, r, http.StatusUnprocessableEntity, agentapi.CodeInvalidInput, "invalid inputs", func(p *agentapi.Problem) { p.Fields = ie.Fields })
		return
	}

	in := a.instance(key)
	in.files.Lock()
	op, status, problem := a.accept(key, spec)
	in.files.Unlock()
	if problem != nil {
		problem(w, r)
		return
	}
	if status == http.StatusAccepted {
		a.log.Info("accepted a desired spec", "instance", key, "generation", spec.Generation,
			"package", spec.Package.String(), "state", spec.State, "operation", op.ID)
		a.wake(key)
	}
	writeJSONResponse(w, status, op)
}

// accept decides on a spec and persists it; the caller holds the
// instance's file lock.
func (a *Agent) accept(key string, spec *agentapi.Spec) (agentapi.Operation, int, func(http.ResponseWriter, *http.Request)) {
	fail := func(status int, code, detail string, extra func(*agentapi.Problem)) (agentapi.Operation, int, func(http.ResponseWriter, *http.Request)) {
		return agentapi.Operation{}, 0, func(w http.ResponseWriter, r *http.Request) { writeProblem(w, r, status, code, detail, extra) }
	}
	internal := func(err error) (agentapi.Operation, int, func(http.ResponseWriter, *http.Request)) {
		return agentapi.Operation{}, 0, func(w http.ResponseWriter, r *http.Request) { a.internalError(w, r, err) }
	}
	dir := a.instanceDir(key)
	st, err := loadState(dir)
	if err != nil {
		return internal(err)
	}
	if st.id != uuid.Nil && st.id != spec.InstanceID {
		return fail(http.StatusConflict, agentapi.CodeInstanceMismatch,
			"the instance directory belongs to another instance (a removed instance's data is retained under this key)", nil)
	}
	if d := st.desired; d != nil {
		switch {
		case spec.Generation < d.Generation:
			return fail(http.StatusConflict, agentapi.CodeStaleGeneration, "a newer generation was accepted",
				func(p *agentapi.Problem) { p.AcceptedGeneration = d.Generation })
		case spec.Generation == d.Generation && spec.Hash() != d.Hash():
			return fail(http.StatusConflict, agentapi.CodeGenerationConflict, "this generation was accepted with other content", nil)
		case spec.Generation == d.Generation:
			op, ok := a.ops.forGeneration(key, spec.Generation)
			if !ok {
				// The operation was pruned; record the outcome again.
				if op, err = a.ops.create(key, opKind(spec), spec.Generation); err != nil {
					return internal(err)
				}
				if st.current != nil && st.current.Generation >= spec.Generation {
					if op, err = a.ops.update(op.ID, func(o *agentapi.Operation) { o.State = agentapi.OpSucceeded }); err != nil {
						return internal(err)
					}
				}
			}
			return op, http.StatusOK, nil
		}
	}
	// Checks compare with what runs (current.json). A removal needs no
	// rendering, so it is accepted for any version.
	if c := st.current; c != nil && spec.State != agentapi.StateAbsent {
		if c.Package.ID != spec.Package.ID {
			return fail(http.StatusUnprocessableEntity, agentapi.CodeInvalidInput, "the instance runs another package",
				func(p *agentapi.Problem) { p.Fields = []string{"package.id"} })
		}
		if c.Package.Version != spec.Package.Version {
			// Package upgrades (snapshot, fenced switch, rollback) come with
			// #60; until then a version change is refused.
			return fail(http.StatusUnprocessableEntity, agentapi.CodeUpgradePathBlocked,
				"this agent does not upgrade packages yet; send the running version",
				func(p *agentapi.Problem) { p.RequiredVersion = c.Package.Version })
		}
	}

	if st.id == uuid.Nil {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return internal(err)
		}
		if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a directory: 0700 is owner-only
			return internal(err)
		}
		bind := struct {
			InstanceID uuid.UUID `json:"instance_id"`
		}{spec.InstanceID}
		if err := writeJSON(filepath.Join(dir, instanceFile), bind); err != nil {
			return internal(err)
		}
	}
	if err := writeSpec(filepath.Join(dir, desiredFile), spec); err != nil {
		return internal(err)
	}
	// Queued operations of older generations never run: only the newest
	// accepted spec is applied.
	for _, old := range a.ops.unfinished() {
		if old.Instance == key && old.State == agentapi.OpQueued && old.Generation < spec.Generation {
			if _, err := a.ops.update(old.ID, func(o *agentapi.Operation) {
				o.State, o.ErrorCode = agentapi.OpFailed, agentapi.CodeSuperseded
			}); err != nil {
				return internal(err)
			}
		}
	}
	op, err := a.ops.create(key, opKind(spec), spec.Generation)
	if err != nil {
		return internal(err)
	}
	return op, http.StatusAccepted, nil
}

func opKind(spec *agentapi.Spec) string {
	if spec.State == agentapi.StateAbsent {
		return agentapi.KindRemove
	}
	return agentapi.KindApply
}

func writeSpec(path string, spec *agentapi.Spec) error { return writeJSON(path, spec) }

// handleLogs answers bounded, redacted service logs.
func (a *Agent) handleLogs(w http.ResponseWriter, r *http.Request) {
	key, ok := pathKey(w, r)
	if !ok {
		return
	}
	in := a.instance(key)
	in.files.Lock()
	st, err := loadState(a.instanceDir(key))
	in.files.Unlock()
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	if st.current == nil {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "the instance is not installed", nil)
		return
	}
	pkg, ok := a.registry.Get(st.current.Package)
	if !ok {
		writeProblem(w, r, http.StatusNotFound, agentapi.CodeNotFound, "unknown package", nil)
		return
	}
	q := r.URL.Query()
	service := q.Get("service")
	tail := 100
	var fields []string
	if !pkg.Manifest.HasService(service) {
		fields = append(fields, "service")
	}
	if raw := q.Get("tail"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > agentapi.MaxLogTail {
			fields = append(fields, "tail")
		}
		tail = n
	}
	since := ""
	if raw := q.Get("since"); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			since = t.UTC().Format(time.RFC3339)
		} else if d, err := time.ParseDuration(raw); err == nil && d > 0 && d <= 31*24*time.Hour {
			since = d.String()
		} else {
			fields = append(fields, "since")
		}
	}
	if len(fields) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, agentapi.CodeInvalidInput, "invalid log query",
			func(p *agentapi.Problem) { p.Fields = fields })
		return
	}
	secrets := st.current.SecretValues()
	if st.desired != nil {
		secrets = append(secrets, st.desired.SecretValues()...)
	}
	c := a.compose(key, secrets)
	out, err := c.logs(r.Context(), service, tail, since)
	if err != nil {
		a.internalError(w, r, err)
		return
	}
	out = redact(out, secrets)
	if len(out) > agentapi.MaxLogBytes {
		out = out[len(out)-agentapi.MaxLogBytes:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}
