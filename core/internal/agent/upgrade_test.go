package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// TestFenceCompose fences a rendered project: no ports, only fenced aliases,
// everything else unchanged; fencing twice changes nothing more.
func TestFenceCompose(t *testing.T) {
	a, _ := newTestAgent(t, time.Now)
	pkg, _ := a.registry.Get(agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version})
	spec := testSpec(1)
	spec.Settings["message"] = `$x "quoted" \ back`
	in, err := pkg.Inputs(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := pkg.Render(spec, in, a.renderContext("git"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(r.Compose), "ports:") {
		t.Fatal("the unfenced render publishes no port")
	}
	fenced, err := fenceCompose(r.Compose)
	if err != nil {
		t.Fatal(err)
	}
	var got, orig map[string]any
	if err := yaml.Unmarshal(fenced, &got); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(r.Compose, &orig); err != nil {
		t.Fatal(err)
	}
	web := got["services"].(map[string]any)["web"].(map[string]any)
	if _, ok := web["ports"]; ok {
		t.Fatal("the fenced project publishes ports")
	}
	aliases := web["networks"].(map[string]any)["edge"].(map[string]any)["aliases"].([]any)
	if len(aliases) != 1 || aliases[0] != packages.ServiceAlias("git", "web", true) {
		t.Fatalf("aliases = %v", aliases)
	}
	// Everything else is unchanged, quoting included.
	delete(orig["services"].(map[string]any)["web"].(map[string]any), "ports")
	orig["services"].(map[string]any)["web"].(map[string]any)["networks"].(map[string]any)["edge"].(map[string]any)["aliases"] = aliases
	if a, b := mustJSON(t, got), mustJSON(t, orig); !bytes.Equal(a, b) {
		t.Fatalf("fencing changed more than ports and aliases:\n%s\n%s", a, b)
	}
	twice, err := fenceCompose(fenced)
	if err != nil || !bytes.Equal(twice, fenced) {
		t.Fatalf("fencing twice: %v\n%s", err, twice)
	}
	// The candidate's own fenced render agrees with the transformation.
	rc := a.renderContext("git")
	rc.Fenced = true
	cand, err := pkg.Render(spec, in, rc)
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err := yaml.Unmarshal(cand.Compose, &c); err != nil {
		t.Fatal(err)
	}
	if a, b := mustJSON(t, c), mustJSON(t, got); !bytes.Equal(a, b) {
		t.Fatalf("fenced render and fenced transformation differ:\n%s\n%s", a, b)
	}
	for _, bad := range []string{"", "[]", "services: [a]", "services:\n  a:\n    networks:\n      e: {aliases: x}\n"} {
		if _, err := fenceCompose([]byte(bad)); err == nil {
			t.Errorf("fenceCompose(%q) accepted", bad)
		}
	}
}

func TestCheckFenced(t *testing.T) {
	for _, c := range []struct {
		json string
		ok   bool
	}{
		{`{"services":{"web":{"networks":{"edge":{"aliases":["k-web-fenced"]},"default":null}}}}`, true},
		{`{"services":{"web":{"ports":[{"target":80}]}}}`, false},
		{`{"services":{"web":{"networks":{"edge":{"aliases":["k-web"]}}}}}`, false},
	} {
		if err := checkFenced([]byte(c.json)); (err == nil) != c.ok {
			t.Errorf("checkFenced(%s) = %v", c.json, err)
		}
	}
}

// TestArchiveRoundTrip archives and extracts a data directory: contents,
// modes, times and symlinks (also ones pointing outside) are kept, and
// nothing is followed.
func TestArchiveRoundTrip(t *testing.T) {
	src := filepath.Join(t.TempDir(), "www")
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(src, "sub", "deep"), 0o755))                  //nolint:gosec // a data tree with typical modes
	must(os.WriteFile(filepath.Join(src, "index.html"), []byte("hello"), 0o640)) //nolint:gosec // see above
	must(os.WriteFile(filepath.Join(src, "sub", "deep", "x.bin"), bytes.Repeat([]byte{7}, 1<<16), 0o600))
	must(os.Symlink(outside, filepath.Join(src, "escape")))
	must(os.Symlink("index.html", filepath.Join(src, "rel")))
	must(os.Chmod(filepath.Join(src, "sub"), 0o555)) //nolint:gosec // see above
	must(os.Chmod(src, 0o750))                       //nolint:gosec // see above
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	must(os.Chtimes(filepath.Join(src, "index.html"), mtime, mtime))

	var buf bytes.Buffer
	must(archiveDir(src, &buf))
	dst := filepath.Join(t.TempDir(), "restored")
	must(os.Mkdir(dst, 0o700))
	must(extractArchive(bytes.NewReader(buf.Bytes()), dst, false))
	// Writable again, so the test directories can be removed.
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(src, "sub"), 0o700) //nolint:gosec // a directory
		_ = os.Chmod(filepath.Join(dst, "sub"), 0o700) //nolint:gosec // a directory
	})

	if b, err := os.ReadFile(filepath.Join(dst, "index.html")); err != nil || string(b) != "hello" { //nolint:gosec // test path
		t.Fatalf("index.html = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dst, "sub", "deep", "x.bin")); err != nil || len(b) != 1<<16 { //nolint:gosec // test path
		t.Fatalf("x.bin: %d bytes, %v", len(b), err)
	}
	for path, want := range map[string]os.FileMode{"": 0o750, "index.html": 0o640, "sub": 0o555, "sub/deep/x.bin": 0o600} {
		if got := fileModeOf(t, filepath.Join(dst, path)); got != want {
			t.Errorf("%s: mode %v, want %v", path, got, want)
		}
	}
	if fi, err := os.Stat(filepath.Join(dst, "index.html")); err != nil || !fi.ModTime().Equal(mtime) {
		t.Errorf("mtime = %v, %v", fi.ModTime(), err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "escape")); err != nil || l != outside {
		t.Errorf("escape link = %q, %v", l, err)
	}
	if l, err := os.Readlink(filepath.Join(dst, "rel")); err != nil || l != "index.html" {
		t.Errorf("rel link = %q, %v", l, err)
	}
	// The archive holds the link, not the file it points to.
	if bytes.Contains(gunzip(t, buf.Bytes()), []byte("outside")) {
		t.Fatal("the archive followed a symlink out of the data directory")
	}
	if n, err := dirSize(src); err != nil || n != 5+1<<16 {
		t.Fatalf("dirSize = %d, %v", n, err)
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := out.ReadFrom(zr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// TestExtractStaysInside: entries that leave the target directory, directly
// or through a symlink, fail and write nothing outside.
func TestExtractStaysInside(t *testing.T) {
	outside := t.TempDir()
	archive := func(entries ...tar.Header) []byte {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		tw := tar.NewWriter(zw)
		for _, h := range entries {
			if h.Typeflag == tar.TypeReg {
				h.Size = 1
			}
			if err := tw.WriteHeader(&h); err != nil {
				t.Fatal(err)
			}
			if h.Typeflag == tar.TypeReg {
				_, _ = tw.Write([]byte("x"))
			}
		}
		_ = tw.Close()
		_ = zw.Close()
		return buf.Bytes()
	}
	for name, data := range map[string][]byte{
		"dotdot":   archive(tar.Header{Name: "../evil", Typeflag: tar.TypeReg, Mode: 0o644}),
		"absolute": archive(tar.Header{Name: filepath.Join(outside, "evil"), Typeflag: tar.TypeReg, Mode: 0o644}),
		"through a symlink": archive(
			tar.Header{Name: "link", Typeflag: tar.TypeSymlink, Linkname: outside},
			tar.Header{Name: "link/evil", Typeflag: tar.TypeReg, Mode: 0o644}),
		"device": archive(tar.Header{Name: "dev", Typeflag: tar.TypeChar, Mode: 0o644}),
	} {
		dst := t.TempDir()
		if err := extractArchive(bytes.NewReader(data), dst, false); err == nil {
			t.Errorf("%s: extracted", name)
		}
		if _, err := os.Stat(filepath.Join(outside, "evil")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s: wrote outside the target", name)
		}
	}
}

// writeTestSnapshot creates a snapshot directory, complete or not.
func writeTestSnapshot(t *testing.T, a *Agent, reason string, complete bool) uuid.UUID {
	t.Helper()
	const key = "git"
	id := uuid.Must(uuid.NewV7())
	dir := a.snapshotPath(key, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	snap := agentapi.Snapshot{Format: agentapi.SnapshotFormat, ID: id, Reason: reason, Package: testSpec(1).Package, Generation: 1, DataDirs: []agentapi.SnapshotDataDir{}}
	if err := writeJSON(filepath.Join(dir, snapshotMeta), snap); err != nil {
		t.Fatal(err)
	}
	if err := writeSpec(filepath.Join(dir, snapshotSpec), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	if complete {
		if err := os.WriteFile(filepath.Join(dir, snapshotComplete), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

// TestSnapshotHousekeeping: partial snapshots go on start, retention keeps
// the newest upgrade snapshots and every manual one, moved-aside data goes
// after 7 days.
func TestSnapshotHousekeeping(t *testing.T) {
	now := time.Now()
	clock := now
	cfg := testConfig(t)
	cfg.SnapshotRetention = 2
	newAgent := func() *Agent {
		a, err := New(Options{Config: cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return clock }, runner: &recordingRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := newAgent()
	if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	manual := writeTestSnapshot(t, a, agentapi.SnapshotReasonManual, true)
	var upgrades []uuid.UUID
	for range 3 {
		upgrades = append(upgrades, writeTestSnapshot(t, a, agentapi.SnapshotReasonUpgrade, true))
	}
	partial := writeTestSnapshot(t, a, agentapi.SnapshotReasonUpgrade, false)

	a = newAgent()
	if _, err := os.Stat(a.snapshotPath("git", partial)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a partial snapshot survived the start")
	}
	list, err := a.listSnapshots("git")
	if err != nil || len(list) != 4 || list[0].ID != upgrades[2] || list[3].ID != manual {
		t.Fatalf("snapshots = %v, %v", list, err)
	}
	if err := a.pruneSnapshots("git"); err != nil {
		t.Fatal(err)
	}
	list, _ = a.listSnapshots("git")
	var ids []uuid.UUID
	for _, s := range list {
		ids = append(ids, s.ID)
	}
	if !slices.Equal(ids, []uuid.UUID{upgrades[2], upgrades[1], manual}) {
		t.Fatalf("after pruning: %v", ids)
	}

	data := filepath.Join(a.instanceDir("git"), dataDir)
	old := uuid.Must(uuid.NewV7())
	if err := os.MkdirAll(filepath.Join(data, movedAside("www", old)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(data, "www"), 0o700); err != nil {
		t.Fatal(err)
	}
	a.cleanAllMovedAside()
	if _, err := os.Stat(filepath.Join(data, movedAside("www", old))); err != nil {
		t.Fatal("fresh moved-aside data was removed")
	}
	clock = now.Add(8 * 24 * time.Hour)
	a.cleanAllMovedAside()
	if _, err := os.Stat(filepath.Join(data, movedAside("www", old))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("moved-aside data older than 7 days was kept")
	}
	if _, err := os.Stat(filepath.Join(data, "www")); err != nil {
		t.Fatal("a data directory was removed")
	}
}

// awaitingAgent prepares an instance whose upgrade waits for confirmation.
func awaitingAgent(t *testing.T, now time.Time) (*Agent, agentapi.Operation) {
	t.Helper()
	a, _ := newTestAgent(t, func() time.Time { return now })
	if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	op, err := a.ops.create("git", agentapi.KindUpgrade, 2)
	if err != nil {
		t.Fatal(err)
	}
	deadline := now.Add(time.Minute)
	j := &journal{Operation: op.ID, Kind: agentapi.KindUpgrade, To: testSpec(2).Package, ToGeneration: 2,
		SnapshotID: uuid.Must(uuid.NewV7()), Phase: agentapi.PhaseAwaitingConfirmation, ConfirmBy: &deadline, Spec: testSpec(2)}
	if err := writeJournal(a.instanceDir("git"), j); err != nil {
		t.Fatal(err)
	}
	a.opPhase(op.ID, agentapi.PhaseAwaitingConfirmation)
	a.instance("git").awaiting = op.ID // the upgrade waits in this process
	return a, op
}

func post(t *testing.T, a *Agent, now time.Time, path string, body []byte) (int, agentapi.Problem, []byte) {
	t.Helper()
	req, mac := signed(t, testKey(t), http.MethodPost, path, body, now)
	rec := serve(a.Handler(), req)
	if !agentapi.VerifyResponse(testKey(t), mac, rec.Code, rec.Header(), rec.Body.Bytes()) {
		t.Fatal("unsigned answer")
	}
	var p agentapi.Problem
	if rec.Code >= 300 {
		_ = json.Unmarshal(rec.Body.Bytes(), &p)
	}
	return rec.Code, p, rec.Body.Bytes()
}

func TestConfirmAndAbort(t *testing.T) {
	now := time.Now()
	a, op := awaitingAgent(t, now)
	confirm, abort := agentapi.OperationConfirmPath(op.ID), agentapi.OperationAbortPath(op.ID)

	if code, p, _ := post(t, a, now, agentapi.OperationConfirmPath(uuid.Must(uuid.NewV7())), nil); code != http.StatusNotFound || p.Code != agentapi.CodeNotFound {
		t.Fatalf("unknown operation: %d %+v", code, p)
	}
	apply, _ := a.ops.forGeneration("git", 1)
	if code, p, _ := post(t, a, now, agentapi.OperationConfirmPath(apply.ID), nil); code != http.StatusConflict || p.Code != agentapi.CodeNotAwaitingConfirmation {
		t.Fatalf("confirm an apply: %d %+v", code, p)
	}

	code, _, body := post(t, a, now, confirm, nil)
	var got agentapi.Operation
	if code != http.StatusOK || json.Unmarshal(body, &got) != nil || got.Phase != agentapi.PhaseCommitting || got.State != agentapi.OpRunning {
		t.Fatalf("confirm: %d %s", code, body)
	}
	j, err := readJournal(a.instanceDir("git"))
	if err != nil || j.Phase != agentapi.PhaseCommitting {
		t.Fatalf("journal after confirm: %+v, %v", j, err)
	}
	select {
	case <-a.instance("git").decision:
	default:
		t.Fatal("the waiting upgrade was not woken")
	}
	// Repeating the confirmation is fine; an abort now is not.
	if code, _, _ := post(t, a, now, confirm, nil); code != http.StatusOK {
		t.Fatalf("second confirm: %d", code)
	}
	if code, p, _ := post(t, a, now, abort, nil); code != http.StatusConflict || p.Code != agentapi.CodeNotAwaitingConfirmation {
		t.Fatalf("abort after confirm: %d %+v", code, p)
	}

	// Abort of another waiting upgrade.
	b, op2 := awaitingAgent(t, now)
	if code, _, _ := post(t, b, now, agentapi.OperationAbortPath(op2.ID), nil); code != http.StatusOK {
		t.Fatalf("abort: %d", code)
	}
	j, _ = readJournal(b.instanceDir("git"))
	if j.Decision != decisionAbort || j.ErrorCode != agentapi.CodeAborted || j.Phase != agentapi.PhaseAwaitingConfirmation {
		t.Fatalf("journal after abort: %+v", j)
	}
	if code, _, _ := post(t, b, now, agentapi.OperationAbortPath(op2.ID), nil); code != http.StatusOK {
		t.Fatalf("second abort: %d", code)
	}
	if code, _, _ := post(t, b, now, agentapi.OperationConfirmPath(op2.ID), nil); code != http.StatusConflict {
		t.Fatalf("confirm after abort: %d", code)
	}
}

// TestConfirmOnlyWhileWaiting: a journal in awaiting_confirmation that no
// upgrade of this process waits on (left by an agent that crashed) cannot
// be confirmed, and neither can an upgrade past its deadline.
func TestConfirmOnlyWhileWaiting(t *testing.T) {
	now := time.Now()
	a, op := awaitingAgent(t, now)
	a.instance("git").awaiting = uuid.Nil // as after a restart, before recovery
	if code, p, _ := post(t, a, now, agentapi.OperationConfirmPath(op.ID), nil); code != http.StatusConflict || p.Code != agentapi.CodeNotAwaitingConfirmation {
		t.Fatalf("confirm of a left-over journal: %d %+v", code, p)
	}
	if j, _ := readJournal(a.instanceDir("git")); j.Phase != agentapi.PhaseAwaitingConfirmation || j.Decision != "" {
		t.Fatalf("journal = %+v", j)
	}

	b, op2 := awaitingAgent(t, now)
	b.now = func() time.Time { return now.Add(2 * time.Minute) }
	if code, p, _ := post(t, b, now, agentapi.OperationConfirmPath(op2.ID), nil); code != http.StatusConflict || p.Code != agentapi.CodeNotAwaitingConfirmation {
		t.Fatalf("confirm after the deadline: %d %+v", code, p)
	}
	if j, _ := readJournal(b.instanceDir("git")); j.Phase != agentapi.PhaseAwaitingConfirmation {
		t.Fatalf("journal after a late confirm = %+v", j)
	}
}

// TestRoutesNeedSignatures: every route refuses unsigned and wrongly signed
// requests before it does anything.
func TestRoutesNeedSignatures(t *testing.T) {
	now := time.Now()
	a, rr := newTestAgent(t, func() time.Time { return now })
	wrong, err := agentapi.DeriveKey(strings.Repeat("w", 40))
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.Must(uuid.NewV7()).String()
	for _, route := range Routes() {
		method, pattern, _ := strings.Cut(route, " ")
		path := strings.NewReplacer("{key}", "git", "{id}", id).Replace(pattern)
		body := []byte(`{"reason":"manual"}`)
		unsigned := httptestRequest(t, method, path, body)
		if rec := serve(a.Handler(), unsigned); rec.Code != http.StatusUnauthorized || rec.Header().Get(agentapi.HeaderSignature) != "" {
			t.Errorf("unsigned %s: %d", route, rec.Code)
		}
		req, _ := signed(t, wrong, method, path, body, now)
		if rec := serve(a.Handler(), req); rec.Code != http.StatusUnauthorized {
			t.Errorf("wrongly signed %s: %d", route, rec.Code)
		}
	}
	if n := rr.count(); n != 0 {
		t.Fatalf("unauthenticated requests invoked Docker %d times", n)
	}
	if _, err := os.Stat(a.instanceDir("git")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("an unauthenticated request created an instance")
	}
}

func httptestRequest(t *testing.T, method, target string, body []byte) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, target, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return req
}

// TestConfirmationTimeout: without a decision the waiting upgrade rolls
// back with confirm_timeout once the deadline passed, and a late
// confirmation is refused.
func TestConfirmationTimeout(t *testing.T) {
	now := time.Now()
	a, op := awaitingAgent(t, now)
	in := a.instance("git")
	j, _ := readJournal(a.instanceDir("git"))
	a.now = func() time.Time { return now.Add(2 * time.Minute) }
	confirmed, err := a.awaitConfirmation(t.Context(), in, j)
	if err != nil || confirmed || j.ErrorCode != agentapi.CodeConfirmTimeout {
		t.Fatalf("await = %v, %v, journal %+v", confirmed, err, j)
	}
	if code, p, _ := post(t, a, now, agentapi.OperationConfirmPath(op.ID), nil); code != http.StatusConflict || p.Code != agentapi.CodeNotAwaitingConfirmation {
		t.Fatalf("late confirm: %d %+v", code, p)
	}
}

func TestSnapshotAndRestoreRequests(t *testing.T) {
	now := time.Now()
	a, rr := newTestAgent(t, func() time.Time { return now })
	snapshot := agentapi.InstanceSnapshotsPath("git")
	restore := agentapi.InstanceRestorePath("git")
	manual := mustJSON(t, agentapi.SnapshotRequest{Reason: agentapi.SnapshotReasonManual})

	if code, p, _ := post(t, a, now, snapshot, manual); code != http.StatusNotFound || p.Code != agentapi.CodeNotFound {
		t.Fatalf("snapshot of an unknown instance: %d %+v", code, p)
	}
	if code, _, _ := post(t, a, now, restore, mustJSON(t, agentapi.RestoreRequest{SnapshotID: uuid.Must(uuid.NewV7()), Generation: 2})); code != http.StatusNotFound {
		t.Fatalf("restore of an unknown instance: %d", code)
	}
	if rec := put(t, a, now, "git", mustJSON(t, testSpec(1))); rec.Code != http.StatusAccepted {
		t.Fatal(rec.Body)
	}
	// Not installed yet.
	if code, _, _ := post(t, a, now, snapshot, manual); code != http.StatusNotFound {
		t.Fatalf("snapshot before install: %d", code)
	}
	if err := writeSpec(filepath.Join(a.instanceDir("git"), currentFile), testSpec(1)); err != nil {
		t.Fatal(err)
	}
	if code, p, _ := post(t, a, now, snapshot, []byte(`{"reason":"upgrade"}`)); code != http.StatusUnprocessableEntity || !slices.Equal(p.Fields, []string{"reason"}) {
		t.Fatalf("snapshot with reason upgrade: %d %+v", code, p)
	}
	code, _, body := post(t, a, now, snapshot, manual)
	var op agentapi.Operation
	if code != http.StatusAccepted || json.Unmarshal(body, &op) != nil || op.Kind != agentapi.KindSnapshot || op.State != agentapi.OpQueued {
		t.Fatalf("snapshot: %d %s", code, body)
	}
	if code, p, _ := post(t, a, now, snapshot, manual); code != http.StatusConflict || p.Code != agentapi.CodeAgentBusy {
		t.Fatalf("second snapshot while one is queued: %d %+v", code, p)
	}
	queued := writeTestSnapshot(t, a, agentapi.SnapshotReasonManual, true)
	if code, p, _ := post(t, a, now, restore, mustJSON(t, agentapi.RestoreRequest{SnapshotID: queued, Generation: 2})); code != http.StatusConflict || p.Code != agentapi.CodeAgentBusy {
		t.Fatalf("restore while a snapshot is queued: %d %+v", code, p)
	}
	a.takeSnapshotRequest(a.instance("git"))
	if err := os.RemoveAll(a.snapshotPath("git", queued)); err != nil {
		t.Fatal(err)
	}

	if code, p, _ := post(t, a, now, restore, []byte(`{}`)); code != http.StatusUnprocessableEntity || !slices.Equal(p.Fields, []string{"snapshot_id", "generation"}) {
		t.Fatalf("empty restore: %d %+v", code, p)
	}
	if code, p, _ := post(t, a, now, restore, mustJSON(t, agentapi.RestoreRequest{SnapshotID: uuid.Must(uuid.NewV7()), Generation: 2})); code != http.StatusNotFound || p.Code != agentapi.CodeNotFound {
		t.Fatalf("restore of an unknown snapshot: %d %+v", code, p)
	}
	id := writeTestSnapshot(t, a, agentapi.SnapshotReasonManual, true)
	if code, p, _ := post(t, a, now, restore, mustJSON(t, agentapi.RestoreRequest{SnapshotID: id, Generation: 1})); code != http.StatusConflict || p.Code != agentapi.CodeStaleGeneration || p.AcceptedGeneration != 1 {
		t.Fatalf("restore with a stale generation: %d %+v", code, p)
	}
	code, _, body = post(t, a, now, restore, mustJSON(t, agentapi.RestoreRequest{SnapshotID: id, Generation: 5}))
	if code != http.StatusAccepted || json.Unmarshal(body, &op) != nil || op.Kind != agentapi.KindRestore || op.Generation != 5 || op.SnapshotID != id.String() {
		t.Fatalf("restore: %d %s", code, body)
	}
	st, err := loadState(a.instanceDir("git"))
	if err != nil || st.desired.Generation != 5 {
		t.Fatalf("desired after the restore request: %+v, %v", st.desired, err)
	}
	j, err := readJournal(a.instanceDir("git"))
	if err != nil || j.Kind != agentapi.KindRestore || j.Phase != agentapi.PhaseRollingBack || j.SnapshotID != id || j.Spec.Generation != 5 {
		t.Fatalf("journal = %+v, %v", j, err)
	}
	// While the restore runs, the instance is busy and rolling back.
	if code, _, _ := post(t, a, now, snapshot, manual); code != http.StatusConflict {
		t.Fatalf("snapshot during a restore: %d", code)
	}
	if n := rr.count(); n != 0 {
		t.Fatalf("requests invoked Docker %d times", n)
	}
	req, _ := signed(t, testKey(t), http.MethodGet, agentapi.InstancePath("git"), nil, now)
	rec := serve(a.Handler(), req)
	var status agentapi.InstanceStatus
	if json.Unmarshal(rec.Body.Bytes(), &status) != nil || status.Phase != agentapi.PhaseRollingBack || status.Snapshots != 1 {
		t.Fatalf("status during a restore: %s", rec.Body)
	}
	req, _ = signed(t, testKey(t), http.MethodGet, agentapi.InstanceSnapshotsPath("git"), nil, now)
	rec = serve(a.Handler(), req)
	var list agentapi.SnapshotList
	if json.Unmarshal(rec.Body.Bytes(), &list) != nil || len(list.Snapshots) != 1 || list.Snapshots[0].ID != id || strings.Contains(rec.Body.String(), "c2VjcmV0LXBhc3N3b3Jk") {
		t.Fatalf("snapshot list: %s", rec.Body)
	}
}

// TestRecoverOperationsLeavesJournals: a restarted agent leaves the
// operation of a journal to journal recovery.
func TestRecoverOperationsLeavesJournals(t *testing.T) {
	now := time.Now()
	a, op := awaitingAgent(t, now)
	b, err := New(Options{Config: a.cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := b.ops.get(op.ID); got.State != agentapi.OpAwaitingConfirmation {
		t.Fatalf("operation after restart = %+v", got)
	}
}

// TestCheckpoint: the crash hook fires only at its point and stops the
// agent.
func TestCheckpoint(t *testing.T) {
	a, _ := newTestAgent(t, time.Now)
	if err := a.checkpoint(agentapi.PhaseVerifying); err != nil {
		t.Fatal(err)
	}
	stopped := false
	a.crashAt, a.stop = agentapi.PhaseVerifying, func() { stopped = true }
	if err := a.checkpoint(agentapi.PhaseMigrating); err != nil || a.crashed.Load() {
		t.Fatal("the hook fired at another point")
	}
	if err := a.checkpoint(agentapi.PhaseVerifying); !errors.Is(err, errCrashed) || !a.crashed.Load() || !stopped {
		t.Fatalf("checkpoint = %v, crashed %v, stopped %v", err, a.crashed.Load(), stopped)
	}
}

// TestExtractKeepsSetuidAsRoot: the owner is applied before the mode, so
// setuid and setgid bits survive a restore by root.
func TestExtractKeepsSetuidAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	src := t.TempDir()
	p := filepath.Join(src, "tool")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(p, 1000, 1000); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o755|os.ModeSetuid|os.ModeSetgid); err != nil { //nolint:gosec // the test's point
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := archiveDir(src, &buf); err != nil {
		t.Fatal(err)
	}
	dst := t.TempDir()
	if err := extractArchive(&buf, dst, true); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dst, "tool"))
	if err != nil || fi.Mode()&(os.ModeSetuid|os.ModeSetgid) != os.ModeSetuid|os.ModeSetgid {
		t.Fatalf("mode after the restore = %v, %v", fi.Mode(), err)
	}
}
