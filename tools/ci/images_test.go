package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Exercise the workflow's pipeline, including its shell selection: losing
// pipefail must not let a failed promotion publish the next image or summary.
func TestImagePromotionStopsOnFailure(t *testing.T) {
	data, err := os.ReadFile("../../.github/workflows/images.yml")
	if err != nil {
		t.Fatal(err)
	}
	var workflow struct {
		Jobs map[string]struct {
			Steps []struct{ Name, Shell, Run string }
		}
	}
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var script, shell string
	for _, step := range workflow.Jobs["promote"].Steps {
		if step.Name == "Move the aliases of both images" {
			script, shell = step.Run, step.Shell
		}
	}
	if script == "" {
		t.Fatal("promotion step not found")
	}
	// GitHub's Linux runner uses bash -e for an unspecified shell; explicit
	// bash also enables pipefail. Keep that distinction in this regression.
	args := []string{"-e"}
	switch shell {
	case "":
	case "bash":
		args = []string{"--noprofile", "--norc", "-e", "-o", "pipefail"}
	default:
		t.Fatalf("unsupported workflow shell %q", shell)
	}
	for _, tt := range []struct {
		name, failImage, wantCalls string
		wantExit                   int
	}{
		{"success", "", "core\nagent\n", 0},
		{"core fails", "core", "core\n", 17},
		{"agent fails", "agent", "core\nagent\n", 17},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			helper := filepath.Join(dir, ".github", "scripts", "promote-aliases.sh")
			if err := os.MkdirAll(filepath.Dir(helper), 0o700); err != nil {
				t.Fatal(err)
			}
			// Inject a failure at the promotion boundary without touching a
			// registry. Output before failing exercises both sed and tee.
			const helperScript = `#!/usr/bin/env bash
echo "$1" >> calls
echo 'latest: promotion attempted'
if [ "$1" = "$FAIL_IMAGE" ]; then exit 17; fi
`
			if err := os.WriteFile(helper, []byte(helperScript), 0o700); err != nil { //nolint:gosec // executable helper in the test's private directory
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", append(args, "-c", script)...) //nolint:gosec // repository workflow in an isolated directory
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "CORE_IMAGE=core", "AGENT_IMAGE=agent", "ALIASES=latest 0.1.0",
				"TAG=v0.1.0", "GITHUB_STEP_SUMMARY="+filepath.Join(dir, "summary"), "FAIL_IMAGE="+tt.failImage)
			out, err := cmd.CombinedOutput()
			code := 0
			if err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("run promotion: %v\n%s", err, out)
				}
				code = exitErr.ExitCode()
			}
			if code != tt.wantExit {
				t.Errorf("exit = %d, want %d\n%s", code, tt.wantExit, out)
			}
			calls, err := os.ReadFile(filepath.Join(dir, "calls")) //nolint:gosec // test's temporary directory
			if err != nil || string(calls) != tt.wantCalls {
				t.Errorf("promoted images = %q (%v), want %q", calls, err, tt.wantCalls)
			}
			summary, err := os.ReadFile(filepath.Join(dir, "summary")) //nolint:gosec // test's temporary directory
			if tt.wantExit != 0 {
				if !os.IsNotExist(err) {
					t.Errorf("failed promotion wrote a summary: %q (%v)", summary, err)
				}
			} else if err != nil || !strings.Contains(string(summary), "- core latest: ") || !strings.Contains(string(summary), "- agent latest: ") {
				t.Errorf("successful promotion summary = %q (%v)", summary, err)
			}
		})
	}
}
