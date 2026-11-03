//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/tests/demotest"
	"github.com/orglane/orglane/tests/pgtest"
)

// TestDemoMode is the acceptance criterion of #12: `orglane server --demo`
// on an empty database starts with the Acme people visible after a
// one-click sign-in.
func TestDemoMode(t *testing.T) {
	dbURL := pgtest.NewDatabase(t)
	p := startServerArgs(t, dbURL, []string{"--demo"})
	logs := p.logs.String()
	if !strings.Contains(logs, "DEMO MODE") || !strings.Contains(logs, "loaded the demo dataset") {
		t.Fatalf("no demo banner in the log:\n%s", logs)
	}
	if tokens := setupTokens(logs); len(tokens) != 0 {
		t.Fatal("demo mode printed a first-run setup token")
	}

	d := demotest.Dataset(t)
	owner := demotest.SignIn(t, p.baseURL, "")
	if owner.Person.Username != d.Owner().Username {
		t.Fatalf("one-click sign-in signed in as %q, want the owner %q", owner.Person.Username, d.Owner().Username)
	}
	assertAcmePeople(t, owner, len(d.People))

	// A cookie-authenticated, state-changing request works like after any
	// sign-in (CSRF token and Origin).
	if code, _, body := owner.Do(t, http.MethodPost, "/api/v1/auth/sign-out", nil); code != http.StatusNoContent {
		t.Fatalf("sign-out = %d %s", code, body)
	}
	if code, _, _ := owner.Get(t, "/api/v1/people"); code != http.StatusUnauthorized {
		t.Fatalf("GET /api/v1/people after sign-out = %d, want 401", code)
	}
	if err := p.stop(t); err != nil {
		t.Fatal(err)
	}

	// A restart in demo mode keeps the dataset and loads nothing again.
	p = startServerArgs(t, dbURL, []string{"--demo"})
	if strings.Contains(p.logs.String(), "loaded the demo dataset") {
		t.Fatal("a restart loaded the demo dataset again")
	}
	assertAcmePeople(t, demotest.SignIn(t, p.baseURL, "priya.raman"), len(d.People))
	if err := p.stop(t); err != nil {
		t.Fatal(err)
	}

	// The same database without demo mode: no one-click sign-in.
	p = startServer(t, dbURL)
	if !strings.Contains(p.logs.String(), "demo mode is off") {
		t.Error("no warning that a demo database runs without demo mode")
	}
	if code, _, _ := post(t, p.baseURL+"/api/v1/demo/sign-in", map[string]string{}); code != http.StatusNotFound {
		t.Fatalf("POST /api/v1/demo/sign-in without demo mode = %d, want 404", code)
	}
}

// TestDemoModeFromConfig: demo.enabled (ORGLANE_DEMO_ENABLED) works like
// --demo.
func TestDemoModeFromConfig(t *testing.T) {
	p := startServer(t, pgtest.NewDatabase(t), "ORGLANE_DEMO_ENABLED=true")
	_, _, body := get(t, p.baseURL+"/.well-known/orglane")
	var disc struct{ Demo bool }
	if err := json.Unmarshal(body, &disc); err != nil || !disc.Demo {
		t.Fatalf("/.well-known/orglane = %s", body)
	}
	demotest.SignIn(t, p.baseURL, "")
}

func assertAcmePeople(t *testing.T, s *demotest.Session, want int) {
	t.Helper()
	code, _, body := s.Get(t, "/api/v1/people")
	var people struct {
		Items []struct {
			Username, DisplayName string
			Title                 *string
			Roles                 []string
			AvatarURL             *string `json:"avatarUrl"`
		}
	}
	if err := json.Unmarshal(body, &people); code != http.StatusOK || err != nil {
		t.Fatalf("GET /api/v1/people = %d %s", code, body)
	}
	if len(people.Items) != want {
		t.Fatalf("%d people visible, want the %d Acme people", len(people.Items), want)
	}
	for _, p := range people.Items {
		if p.Title == nil || len(p.Roles) != 1 || p.AvatarURL == nil {
			t.Errorf("%s: missing title, role or avatar: %+v", p.Username, p)
		}
	}
	code, header, _ := s.Get(t, *people.Items[0].AvatarURL)
	if code != http.StatusOK || header.Get("Content-Type") != "image/svg+xml" || header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("avatar = %d %v", code, header)
	}
}

// TestDemoModeRefusesNonEmptyDatabase: demo mode never touches a database
// with real data; the server exits with an error.
func TestDemoModeRefusesNonEmptyDatabase(t *testing.T) {
	dbURL := pgtest.NewDatabase(t)
	p := startServer(t, dbURL)
	tokens := setupTokens(p.logs.String())
	if len(tokens) != 1 {
		t.Fatalf("setup tokens = %v", tokens)
	}
	setup := map[string]string{"setupToken": tokens[0], "workEmail": "owner@acme.test", "username": "owner", "displayName": "Owner", "password": "correct horse battery staple"}
	if code, _, body := post(t, p.baseURL+"/api/v1/auth/setup", setup); code != http.StatusCreated {
		t.Fatalf("setup = %d %s", code, body)
	}
	if err := p.stop(t); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary(t), "server", "--demo") //nolint:gosec // the binary built by binary()
	cmd.Env = serverEnv(dbURL, freeAddr(t))
	out, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("server --demo on a non-empty database: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "demo mode needs an empty database") {
		t.Fatalf("the error does not explain why:\n%s", out)
	}
	if strings.Contains(string(out), "DEMO MODE") {
		t.Fatal("the demo banner was logged although demo mode was refused")
	}
}
