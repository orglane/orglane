//go:build integration

package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// TestInstanceNamesWithDocker checks #88 with real Docker (docs/specs/agent.md
// "Instance names"): the keys "nx" and "nx-web" produced the same names
// before service names lost the "-" (nx-web's worker and nx's "web-worker"
// were both the alias nx-web-worker and the container <project>-nx-web-worker-1).
// The spelling with the "-" is refused before anything runs; both instances
// run side by side, each routed alias reaches only its own instance, and no
// name Docker's DNS gives one instance's containers (other than the bare
// service names, which Orglane never routes) belongs to the other.
func TestInstanceNamesWithDocker(t *testing.T) {
	t.Parallel() // its own bundle; overlaps the other long Docker tests
	holdDockerSlot(t)
	project := startBundle(t)
	keys := []string{"nx", "nx-web"}
	for _, key := range keys {
		cleanupProject(t, project, key)
	}

	// nx with a service web-worker: refused by the policy check.
	src, rule, err := agenttest.PolicyVariant("dash_in_service")
	if err != nil {
		t.Fatal(err)
	}
	variants, err := packages.NewRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	refusing := startAgent(t, project, variants)
	op := refusing.apply("nx", integrationSpec(uuid.Must(uuid.NewV7()), 1, freePort(t), "refused", agentapi.StateRunning))
	if op.State != agentapi.OpFailed || op.ErrorCode != agentapi.CodePolicyViolation || op.Rule != rule || rule != packages.RuleAlias {
		t.Fatalf("operation = %+v, want policy_violation %s", op, packages.RuleAlias)
	}
	if c := refusing.containers("nx"); len(c) != 0 {
		t.Fatalf("containers of a refused project: %v", c)
	}
	if _, err := os.Stat(filepath.Join(refusing.agent.instanceDir("nx"), composeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a refused project was written")
	}
	// It would keep reconciling its nx: stop it before the real nx runs.
	if err := refusing.stop(); err != nil {
		t.Fatal(err)
	}

	registry, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	e := startAgent(t, project, registry)
	for _, key := range keys {
		e.mustApplyKey(key, integrationSpec(uuid.Must(uuid.NewV7()), 1, freePort(t), "message of "+key, agentapi.StateRunning))
	}

	// Each routed alias reaches its own instance through the edge network.
	proxy := docker(t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+project, "--filter", "label=com.docker.compose.service=traefik")
	for _, key := range keys {
		alias := packages.ServiceAlias(key, "web", false)
		eventually(t, time.Minute, alias+" answers", func() bool {
			out, err := exec.CommandContext(t.Context(), "docker", "exec", proxy, "wget", "-q", "-T", "3", "-O", "-", "http://"+alias+":8080/").CombinedOutput() //nolint:gosec // test commands
			return err == nil && strings.TrimSpace(string(out)) == "message of "+key
		})
	}

	// The names Docker registers for the containers of each instance.
	own := map[string][]string{}
	for _, key := range keys {
		for _, ct := range e.containerDNS(key) {
			for _, names := range ct.DNSNames {
				for _, n := range names {
					if n != ct.Service {
						own[key] = append(own[key], n)
					}
				}
			}
		}
		for _, want := range []string{packages.ServiceAlias(key, "web", false), packages.InstanceProject(project, key) + "-worker-1"} {
			if !slices.Contains(own[key], want) {
				t.Fatalf("%s does not answer to %s: %v", key, want, own[key])
			}
		}
	}
	for _, n := range own["nx"] {
		if slices.Contains(own["nx-web"], n) {
			t.Fatalf("both instances answer to %s", n)
		}
	}
}
