package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
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
	for _, args := range [][]string{{"server"}, {"cli", "migrate"}, {"cli", "healthcheck"}} {
		code, _, errOut := runArgs(t, args...)
		if code != 1 || !strings.Contains(errOut, "ORGLANE_DATABASE_URL") || !strings.Contains(errOut, "ORGLANE_DOMAIN_BASE") {
			t.Errorf("%v: code %d, stderr %q; want 1 with config errors", args, code, errOut)
		}
	}
}

func TestHealthcheck(t *testing.T) {
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	vars := map[string]string{
		"ORGLANE_DATABASE_URL":  "postgres://orglane@postgres/orglane",
		"ORGLANE_DOMAIN_BASE":   "acme.com",
		"ORGLANE_SERVER_LISTEN": srv.Listener.Addr().String(),
		// Required in production; the healthcheck itself never uses it.
		"ORGLANE_IDENTITY_MASTER_SECRET": "test-master-secret-0123456789abcdef",
	}
	check := func() (int, string, string) {
		var out, errOut bytes.Buffer
		code := run(t.Context(), []string{"cli", "healthcheck"}, &out, &errOut, func(k string) (string, bool) {
			v, ok := vars[k]
			return v, ok
		})
		return code, out.String(), errOut.String()
	}

	if code, out, errOut := check(); code != 0 || out != "ready\n" {
		t.Errorf("ready server: code %d, stdout %q, stderr %q", code, out, errOut)
	}
	status = http.StatusServiceUnavailable
	if code, _, errOut := check(); code != 1 || !strings.Contains(errOut, "503") {
		t.Errorf("unready server: code %d, stderr %q; want 1", code, errOut)
	}
	srv.Close()
	if code, _, errOut := check(); code != 1 || !strings.Contains(errOut, "healthcheck") {
		t.Errorf("stopped server: code %d, stderr %q; want 1", code, errOut)
	}
}

func TestLoopbackAddr(t *testing.T) {
	for listen, want := range map[string]string{
		":8080":          "127.0.0.1:8080",
		"0.0.0.0:8080":   "127.0.0.1:8080",
		"[::]:8080":      "[::1]:8080",
		"10.0.0.5:8080":  "10.0.0.5:8080",
		"localhost:8080": "localhost:8080",
	} {
		if got := loopbackAddr(listen); got != want {
			t.Errorf("loopbackAddr(%q) = %q, want %q", listen, got, want)
		}
	}
}
