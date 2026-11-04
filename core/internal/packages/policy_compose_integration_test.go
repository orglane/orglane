//go:build integration

package packages

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/orglane/orglane/core/internal/agentapi"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// composeConfig writes a rendered project into rc.InstanceDir and returns
// `docker compose config --format json` of it, as the agent runs it.
func composeConfig(t *testing.T, r *Rendered, rc RenderContext) []byte {
	t.Helper()
	if err := os.MkdirAll(rc.DataPath("www"), 0o750); err != nil {
		t.Fatal(err)
	}
	for name, f := range r.Files {
		p := filepath.Join(rc.FilesPath(), filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, f.Mode); err != nil {
			t.Fatal(err)
		}
	}
	compose, env := filepath.Join(rc.InstanceDir, "compose.yaml"), filepath.Join(rc.InstanceDir, ".env")
	if err := os.WriteFile(compose, r.Compose, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env, r.Env, 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"compose", "--project-name", rc.Project(), "--project-directory", rc.InstanceDir,
		"-f", compose, "--env-file", env, "config", "--format", "json"}
	cmd := exec.CommandContext(t.Context(), "docker", args...) //nolint:gosec // test paths
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker compose config: %v: %s", err, stderr.String())
	}
	return out
}

// TestPolicyWithCompose renders the agenttest package and its policy
// variants, normalizes them with the real `docker compose config` and checks
// that the policy accepts the package and refuses every variant by its rule.
func TestPolicyWithCompose(t *testing.T) {
	check := func(t *testing.T, src *Registry, rc RenderContext) error {
		t.Helper()
		p, ok := src.Get(agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version})
		if !ok {
			t.Fatal("package not found")
		}
		spec := testSpec()
		in, err := p.Inputs(spec)
		if err != nil {
			t.Fatal(err)
		}
		r, err := p.Render(spec, in, rc)
		if err != nil {
			t.Fatal(err)
		}
		return p.CheckPolicy(composeConfig(t, r, rc), in, rc)
	}

	for _, fenced := range []bool{false, true} {
		rc := testContext(t)
		rc.Fenced = fenced
		if err := check(t, testRegistry(t), rc); err != nil {
			t.Fatalf("agenttest (fenced %v): %v", fenced, err)
		}
	}

	for _, name := range agenttest.PolicyVariants() {
		t.Run(name, func(t *testing.T) {
			src, rule, err := agenttest.PolicyVariant(name)
			if err != nil {
				t.Fatal(err)
			}
			r, err := NewRegistry(src)
			if err != nil {
				t.Fatal(err)
			}
			err = check(t, r, testContext(t))
			var pe *PolicyError
			if !errors.As(err, &pe) || pe.Rule != rule {
				t.Fatalf("error = %v, want rule %s", err, rule)
			}
		})
	}
}
