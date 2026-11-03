package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func noEnv(string) (string, bool) { return "", false }

func runArgs(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = run(context.Background(), args, &out, &errOut, noEnv)
	return code, out.String(), errOut.String()
}

func TestVersion(t *testing.T) {
	for _, args := range [][]string{{"cli", "version"}, {"--version"}} {
		code, out, _ := runArgs(t, args...)
		if code != 0 || out != "orglane "+version+"\n" {
			t.Errorf("%v: code %d, output %q", args, code, out)
		}
	}
}

func TestAgentStub(t *testing.T) {
	code, _, errOut := runArgs(t, "agent")
	if code != 0 || !strings.Contains(errOut, "not implemented") {
		t.Fatalf("agent: code %d, stderr %q", code, errOut)
	}
}

func TestHelp(t *testing.T) {
	code, out, _ := runArgs(t, "help")
	if code != 0 || !strings.Contains(out, "Usage: orglane") {
		t.Fatalf("help: code %d, output %q", code, out)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"serve"},
		{"cli"},
		{"cli", "frobnicate"},
		{"server", "-bogus"},
		{"cli", "migrate", "extra"},
	} {
		code, _, errOut := runArgs(t, args...)
		if code != 2 || !strings.Contains(errOut, "Usage: orglane") {
			t.Errorf("%v: code %d, stderr %q; want 2 with usage", args, code, errOut)
		}
	}
}

func TestInvalidConfigIsReported(t *testing.T) {
	for _, args := range [][]string{{"server"}, {"cli", "migrate"}} {
		code, _, errOut := runArgs(t, args...)
		if code != 1 || !strings.Contains(errOut, "ORGLANE_DATABASE_URL") || !strings.Contains(errOut, "ORGLANE_DOMAIN_BASE") {
			t.Errorf("%v: code %d, stderr %q; want 1 with config errors", args, code, errOut)
		}
	}
}
