package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// TestFailedStateWriteIsNotSuccess: when current.json cannot be written
// after a successful converge, the operation fails, the generation stays
// pending (also after a restart), and a later attempt succeeds.
func TestFailedStateWriteIsNotSuccess(t *testing.T) {
	now := time.Now()
	cfg := testConfig(t)
	newAgent := func() *Agent {
		a, err := New(Options{Config: cfg, Version: "dev", Registry: testRegistry(t), Now: func() time.Time { return now }, runner: &recordingRunner{}})
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	a := newAgent()
	rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	var op agentapi.Operation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	dir := a.instanceDir("git")
	// A non-empty directory where current.json goes: the rename fails.
	if err := os.MkdirAll(filepath.Join(dir, currentFile, "obstacle"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := loadStateIgnoringCurrent(dir)
	if err != nil {
		t.Fatal(err)
	}
	in := a.instance("git")
	if code := a.finish(in, st, st.desired, &op, nil); code != agentapi.CodeInternal {
		t.Fatalf("finish with an unwritable current.json = %q, want internal", code)
	}
	if got, _ := a.ops.get(op.ID); got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodeInternal {
		t.Fatalf("operation = %+v, want failed", got)
	}

	// After a restart the generation is still pending.
	if err := os.RemoveAll(filepath.Join(dir, currentFile)); err != nil {
		t.Fatal(err)
	}
	b := newAgent()
	st, err = loadState(dir)
	if err != nil || !st.pending() || st.current != nil {
		t.Fatalf("state after restart: pending %v, current %+v, %v", st.pending(), st.current, err)
	}
	retry, err := b.ops.create("git", agentapi.KindApply, 1)
	if err != nil {
		t.Fatal(err)
	}
	if code := b.finish(b.instance("git"), st, st.desired, &retry, nil); code != "" {
		t.Fatalf("retry = %q", code)
	}
	if got, _ := b.ops.get(retry.ID); got.State != agentapi.OpSucceeded {
		t.Fatalf("retry operation = %+v", got)
	}
	if st, err = loadState(dir); err != nil || st.pending() || st.current == nil || st.current.Generation != 1 {
		t.Fatalf("state after the retry: %+v, %v", st, err)
	}
}

// TestFailedBlockedWriteKeepsCurrent: when blocked.json cannot be written,
// current.json does not advance and the operation fails as internal.
func TestFailedBlockedWriteKeepsCurrent(t *testing.T) {
	now := time.Now()
	a, _ := newTestAgent(t, func() time.Time { return now })
	rec := put(t, a, now, "git", mustJSON(t, testSpec(1)))
	var op agentapi.Operation
	if err := json.Unmarshal(rec.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	dir := a.instanceDir("git")
	if err := os.MkdirAll(filepath.Join(dir, blockedFile, "obstacle"), 0o700); err != nil {
		t.Fatal(err)
	}
	st, err := loadStateIgnoringCurrent(dir)
	if err != nil {
		t.Fatal(err)
	}
	if code := a.finish(a.instance("git"), st, st.desired, &op, nil); code != agentapi.CodeInternal {
		t.Fatalf("finish with an unwritable blocked.json = %q, want internal", code)
	}
	if got, _ := a.ops.get(op.ID); got.State != agentapi.OpFailed || got.ErrorCode != agentapi.CodeInternal {
		t.Fatalf("operation = %+v, want failed", got)
	}
	if _, err := os.Stat(filepath.Join(dir, currentFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current.json advanced although blocked.json failed: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, blockedFile)); err != nil {
		t.Fatal(err)
	}
	if st, err = loadState(dir); err != nil || !st.pending() {
		t.Fatalf("generation not pending: %v", err)
	}
}

// loadStateIgnoringCurrent reads the state while current.json is an
// obstacle directory.
func loadStateIgnoringCurrent(dir string) (*instanceState, error) {
	st := &instanceState{}
	var err error
	st.desired, err = readSpec(filepath.Join(dir, desiredFile))
	return st, err
}

func TestConfiguredHash(t *testing.T) {
	dir := t.TempDir()
	if h := configuredHash(dir); h != "" {
		t.Fatalf("fresh directory: %q", h)
	}
	if err := writeConfigured(dir, "abc"); err != nil {
		t.Fatal(err)
	}
	if h := configuredHash(dir); h != "abc" {
		t.Fatalf("configured hash = %q", h)
	}
	if fileModeOf(t, filepath.Join(dir, configuredFile)) != 0o600 {
		t.Fatal("configured.json is not 0600")
	}
}

func fileModeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm()
}

func TestTailBuffer(t *testing.T) {
	tb := newTailBuffer(10)
	for _, s := range []string{"abc", "defgh", "ijklmnop", "q"} {
		if _, err := tb.Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
	}
	if got := string(tb.Bytes()); got != "hijklmnopq" || !tb.Truncated() {
		t.Fatalf("tail = %q, truncated %v", got, tb.Truncated())
	}
	tb = newTailBuffer(10)
	_, _ = tb.Write([]byte("short"))
	if got, cut := tb.text(); string(got) != "short" || cut {
		t.Fatalf("untruncated = %q, %v", got, cut)
	}
	tb = newTailBuffer(8)
	_, _ = tb.Write([]byte(strings.Repeat("x", 100)))
	if got, cut := tb.text(); len(got) != 0 || !cut {
		t.Fatalf("one long cut line = %q, %v", got, cut)
	}
	tb = newTailBuffer(8)
	_, _ = tb.Write([]byte("SECRET-VALUE\nok\n"))
	if got, _ := tb.text(); string(got) != "ok\n" {
		t.Fatalf("cut first line kept: %q", got)
	}
}

func TestBoundLogs(t *testing.T) {
	secret := "c2VjcmV0LXBhc3N3b3Jk" //nolint:gosec // a test secret
	var in bytes.Buffer
	for range 2000 {
		in.WriteString("2026-11-04T00:00:00Z line with " + secret + " inside\n")
	}
	out := boundLogs(in.Bytes(), false, []string{secret}, 4096)
	if len(out) > 4096 || bytes.Contains(out, []byte(secret)) || !bytes.HasPrefix(out, []byte(truncatedMarker)) {
		t.Fatalf("bounded logs: %d bytes, prefix %q", len(out), out[:40])
	}
	for _, line := range strings.Split(strings.TrimSuffix(string(out[len(truncatedMarker):]), "\n"), "\n") {
		if !strings.HasPrefix(line, "2026-") {
			t.Fatalf("a cut line was kept: %q", line)
		}
	}
	if got := boundLogs([]byte("a "+secret+"\n"), false, []string{secret}, 4096); string(got) != "a [redacted]\n" {
		t.Fatalf("small logs = %q", got)
	}
}

// fakeDocker writes a script that stands in for the docker binary.
func fakeDocker(t *testing.T, script string) *dockerCLI {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docker")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil { //nolint:gosec // a test executable
		t.Fatal(err)
	}
	return &dockerCLI{bin: path, env: []string{"PATH=" + os.Getenv("PATH")}}
}

// TestDockerOutputIsBounded: oversized output (one 8 MiB line without a
// newline, then a secret cut by the tail limit, then normal lines) is
// drained, but only a bounded tail is kept, and no fragment of the secret
// survives the cut.
func TestDockerOutputIsBounded(t *testing.T) {
	const secret = "c2VjcmV0LXBhc3N3b3Jk" //nolint:gosec // a test secret
	d := fakeDocker(t, `head -c 8388608 /dev/zero | tr '\0' 'a'
printf 'xx%s\n' "`+secret+`"
i=0; while [ $i -lt 100 ]; do echo "line $i"; i=$((i+1)); done
head -c 1048576 /dev/zero | tr '\0' 'e' >&2`)
	limit := 10*len("line 0\n") + 90*len("line 10\n") + 10 // cuts inside the secret's line
	out, truncated, stderr, err := d.tail(t.Context(), limit, "logs")
	if err != nil {
		t.Fatal(err)
	}
	if !truncated || len(out) > limit || bytes.Contains(out, []byte("c2V")) || !bytes.HasPrefix(out, []byte("line 0\n")) {
		t.Fatalf("tail: truncated %v, %d bytes, starts %q", truncated, len(out), out[:min(20, len(out))])
	}
	if len(stderr) > maxStderr {
		t.Fatalf("stderr kept %d bytes", len(stderr))
	}

	big := fakeDocker(t, `head -c 20000000 /dev/zero`)
	if out, _, err := big.run(t.Context(), nil, "config"); !errors.Is(err, errOutputTooLarge) || len(out) > maxCommandOutput {
		t.Fatalf("oversized output: %v, %d bytes", err, len(out))
	}
}
