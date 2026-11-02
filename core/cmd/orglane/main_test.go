package main

import (
	"bytes"
	"testing"
)

func TestRunPrintsVersion(t *testing.T) {
	var out bytes.Buffer

	if code := run(&out); code != 0 {
		t.Fatalf("run() exit code = %d, want 0", code)
	}

	want := "orglane " + version + "\n"
	if got := out.String(); got != want {
		t.Fatalf("run() output = %q, want %q", got, want)
	}
}
