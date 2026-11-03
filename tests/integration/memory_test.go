//go:build integration

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/tests/pgtest"
)

// idleSettle is how long the server sits idle before RSS is sampled.
const idleSettle = 5 * time.Second

// TestIdleMemory reports the resident memory of an idle server (ADR 0009).
// It is a measurement, not a gate: it never fails on the number. Set
// ORGLANE_MEMORY_REPORT to a file path (for example $GITHUB_STEP_SUMMARY) to
// append the result as Markdown.
func TestIdleMemory(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skip("ps not available")
	}
	p := startServer(t, pgtest.NewDatabase(t))
	time.Sleep(idleSettle)

	pid := strconv.Itoa(p.cmd.Process.Pid)
	out, err := exec.CommandContext(t.Context(), "ps", "-o", "rss=", "-p", pid).Output() //nolint:gosec // the PID of the server under test
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	kib, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("parse ps output %q: %v", out, err)
	}
	mib := float64(kib) / 1024
	line := fmt.Sprintf("idle RSS of `orglane server` after %s: %.1f MiB (%d KiB)", idleSettle, mib, kib)
	t.Log(line)
	fmt.Println("orglane-memory:", line)

	if path := os.Getenv("ORGLANE_MEMORY_REPORT"); path != "" {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //nolint:gosec // path chosen by the operator
		if err != nil {
			t.Fatalf("open memory report: %v", err)
		}
		defer func() { _ = f.Close() }()
		if _, err := fmt.Fprintf(f, "### Idle memory\n\n%s\n", line); err != nil {
			t.Fatal(err)
		}
	}

	if err := p.stop(t); err != nil {
		t.Errorf("exit after SIGTERM: %v", err)
	}
}
