package logging

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestJSON(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, "json", "info").Info("hello", "k", "v")
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("not JSON: %q", buf.String())
	}
	if rec["msg"] != "hello" || rec["k"] != "v" {
		t.Fatalf("record = %v", rec)
	}
}

func TestTextAndLevel(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "text", "warn")
	l.Info("hidden")
	l.Warn("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "msg=shown") {
		t.Fatalf("output = %q", out)
	}
}

func TestUnknownLevelIsInfo(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, "text", "loud")
	l.Debug("hidden")
	l.Info("shown")
	if out := buf.String(); strings.Contains(out, "hidden") || !strings.Contains(out, "shown") {
		t.Fatalf("output = %q", out)
	}
}
