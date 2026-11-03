//go:build integration

package integration

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/orglane/orglane/tests/pgtest"
)

// setupTokens returns the setup tokens printed in the server's JSON log.
func setupTokens(logs string) []string {
	var tokens []string
	sc := bufio.NewScanner(strings.NewReader(logs))
	for sc.Scan() {
		var line struct {
			Token string `json:"setup_token"`
		}
		if json.Unmarshal(sc.Bytes(), &line) == nil && line.Token != "" {
			tokens = append(tokens, line.Token)
		}
	}
	return tokens
}

func cli(t *testing.T, dbURL string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary(t), append([]string{"cli"}, args...)...) //nolint:gosec // the binary built by binary()
	cmd.Env = serverEnv(dbURL, "127.0.0.1:0")
	out, err := cmd.Output()
	if err != nil {
		var stderr []byte
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			stderr = exitErr.Stderr
		}
		t.Fatalf("orglane cli %v: %v\n%s%s", args, err, out, stderr)
	}
	return string(out)
}

// post sends JSON and returns the status, headers and body.
func post(t *testing.T, url string, body any) (int, http.Header, []byte) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, b
}

func TestFirstRunSetup(t *testing.T) {
	dbURL := pgtest.NewDatabase(t)
	p := startServer(t, dbURL)
	tokens := setupTokens(p.logs.String())
	if len(tokens) != 1 || !strings.HasPrefix(tokens[0], "ost_") {
		t.Fatalf("setup tokens in the log = %v, want exactly one", tokens)
	}
	first := tokens[0]

	// Discovery names the workspace and the issuer, nothing else.
	_, _, body := get(t, p.baseURL+"/.well-known/orglane")
	var disc map[string]any
	if err := json.Unmarshal(body, &disc); err != nil || disc["issuer"] != "https://work.acme.test" || disc["apiBaseUrl"] != "https://work.acme.test/api/v1" ||
		disc["demo"] != false || len(disc) != 5 {
		t.Fatalf("/.well-known/orglane = %s", body)
	}
	_, _, body = get(t, p.baseURL+"/.well-known/openid-configuration")
	if !strings.Contains(string(body), `"issuer":"https://work.acme.test"`) {
		t.Fatalf("openid-configuration = %s", body)
	}
	if err := p.stop(t); err != nil {
		t.Fatal(err)
	}

	// A restart does not print another token; the CLI replaces it.
	p = startServer(t, dbURL)
	if tokens := setupTokens(p.logs.String()); len(tokens) != 0 {
		t.Fatalf("a restart printed setup tokens again: %v", tokens)
	}
	out := cli(t, dbURL, "setup-token")
	second := regexp.MustCompile(`ost_[A-Za-z0-9_-]{43}`).FindString(out)
	if second == "" || second == first {
		t.Fatalf("cli setup-token printed %q", out)
	}

	setup := map[string]string{"workEmail": "owner@acme.test", "username": "owner", "displayName": "Owner", "password": "correct horse battery staple"}
	setup["setupToken"] = first
	if code, _, body := post(t, p.baseURL+"/api/v1/auth/setup", setup); code != http.StatusForbidden {
		t.Fatalf("setup with the replaced token = %d %s", code, body)
	}
	setup["setupToken"] = second
	code, header, body := post(t, p.baseURL+"/api/v1/auth/setup", setup)
	if code != http.StatusCreated {
		t.Fatalf("setup = %d %s", code, body)
	}
	cookie := strings.Join(header.Values("Set-Cookie"), "\n")
	for _, want := range []string{"__Host-orglane_session=ols_", "Path=/", "HttpOnly", "Secure", "SameSite=Lax"} {
		if !strings.Contains(cookie, want) {
			t.Errorf("Set-Cookie lacks %q:\n%s", want, cookie)
		}
	}
	if strings.Contains(strings.ToLower(cookie), "domain=") {
		t.Errorf("session cookie has a Domain attribute (must be host-only):\n%s", cookie)
	}

	// Clients are registered with the CLI.
	out = cli(t, dbURL, "oidc-client", "add", "-id", "forgejo", "-name", "Forgejo", "-redirect-uri", "https://git.acme.test/user/oauth2/orglane/callback")
	if !strings.Contains(out, "client_id: forgejo") || !regexp.MustCompile(`client_secret: olcs_\S{43}`).MatchString(out) {
		t.Fatalf("cli oidc-client add printed %q", out)
	}
	if err := p.stop(t); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(p.logs.String(), "olcs_") || strings.Contains(p.logs.String(), "correct horse") {
		t.Error("the server log leaks a client secret or password")
	}
}
