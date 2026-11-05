//go:build integration

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// Write fence tests with real Docker (docs/specs/agent.md "Upgrade", fence.go):
// no name in Docker's DNS that a route of the instance may target reaches a
// fenced project, however the compose file spells it.
func (s *upgradeSuite) fence(t *testing.T) {
	s.run(t, "fence-implicit-service-name", s.testImplicitServiceName)
	for _, c := range []struct {
		name, key string
		// legacy rewrites the running deployment (the routed alias
		// "<key>-web" is passed in) as a release this one did not render
		// could have written it.
		legacy func(compose []byte, routed string) []byte
	}{
		// An explicit routed alias that is the candidate's fenced alias.
		{"explicit-alias", "lgexpl", func(compose []byte, routed string) []byte {
			return bytes.ReplaceAll(compose, []byte(strconv.Quote(routed)), []byte(strconv.Quote(routed+packages.FencedSuffix)))
		}},
		// A routed name that is the candidate's service name, which Docker
		// registers without an explicit alias.
		{"implicit-name", "lgimpl", func(compose []byte, routed string) []byte {
			return bytes.ReplaceAll(compose, []byte(strconv.Quote(routed)), []byte(strconv.Quote(routed)+", \"web\""))
		}},
	} {
		s.run(t, "fence-legacy-"+c.name, func(t *testing.T) { s.testLegacyRouteCollision(t, c.key, c.legacy) })
	}
	s.run(t, "fence-dns-names", s.testFencedDNSNames)
	s.run(t, "fence-anchored-snapshot", func(t *testing.T) { s.testAnchoredSnapshot(t, "ancw", false) })
	s.run(t, "fence-anchored-legacy-snapshot", func(t *testing.T) { s.testAnchoredSnapshot(t, "ancl", true) })
}

// containerNames is what Docker reports for a container of an instance.
type containerNames struct {
	Name, Service, Hostname, ID string
	// DNSNames per network: every name Docker's DNS answers for it.
	DNSNames map[string][]string
}

func (e *testEnv) containerDNS(key string) []containerNames {
	e.t.Helper()
	ids := e.containers(key)
	if len(ids) == 0 {
		e.t.Fatal("no containers")
	}
	var raw []struct {
		ID     string `json:"Id"`
		Name   string
		Config struct {
			Hostname string
			Labels   map[string]string
		}
		NetworkSettings struct {
			Networks map[string]struct {
				DNSNames []string
			}
		}
	}
	if err := json.Unmarshal([]byte(docker(e.t, append([]string{"inspect"}, ids...)...)), &raw); err != nil {
		e.t.Fatal(err)
	}
	var out []containerNames
	for _, r := range raw {
		c := containerNames{
			Name: strings.TrimPrefix(r.Name, "/"), Service: r.Config.Labels["com.docker.compose.service"],
			Hostname: r.Config.Hostname, ID: r.ID[:12], DNSNames: map[string][]string{},
		}
		for net, n := range r.NetworkSettings.Networks {
			if n.DNSNames == nil {
				e.t.Fatalf("Docker reports no DNSNames for %s on %s (Docker 25 or newer is needed)", c.Name, net)
			}
			c.DNSNames[net] = n.DNSNames
		}
		out = append(out, c)
	}
	return out
}

// assertFencedNames checks a running fenced project with real Docker: the
// names Docker's DNS gives its containers are exactly those fence.go
// models (explicit aliases of the deployed project, service name, container
// name, hostname, short ID), none of them is routed, and no routed name
// answers from the edge network.
func (e *testEnv) assertFencedNames(key string, routed []string) {
	e.t.Helper()
	c := e.agent.compose(key, nil)
	normalized, err := c.config(e.t.Context(), filepath.Join(c.dir, composeFile), filepath.Join(c.dir, envFile))
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := parseDNSProject(normalized)
	if err != nil {
		e.t.Fatal(err)
	}
	if err := checkFenced(normalized, c.project, routed); err != nil {
		e.t.Fatalf("the deployed project is not fenced: %v", err)
	}
	var explicit []string
	for _, s := range p.Services {
		for _, n := range s.Networks {
			if n != nil {
				explicit = append(explicit, n.Aliases...)
			}
		}
	}
	for _, ct := range e.containerDNS(key) {
		modeled := append([]string{ct.Name, ct.Service, ct.Hostname, ct.ID}, explicit...)
		for net, names := range ct.DNSNames {
			for _, n := range names {
				if !slices.Contains(modeled, n) {
					e.t.Fatalf("Docker registers %s for %s on %s, which the fence does not model", n, ct.Name, net)
				}
				if slices.Contains(routed, n) {
					e.t.Fatalf("the fenced container %s answers to the routed name %s on %s", ct.Name, n, net)
				}
			}
		}
	}
	for _, r := range routed {
		if out, err := e.edgeVersion(r); err == nil {
			e.t.Fatalf("the routed name %s reaches the fenced project: %q", r, out)
		}
	}
}

// A service renamed to another version's routed alias (key "implicit":
// v1 "web" routes through implicit-web, v2 calls a service implicit-web)
// would answer that alias through Docker's service-name DNS entry. The
// upgrade is refused before anything stops; the write acknowledged through
// the old route afterwards is kept.
func (s *upgradeSuite) testImplicitServiceName(t *testing.T) {
	const key = "implicit"
	cleanupProject(t, s.project, key)
	src := fstest.MapFS{}
	if err := fs.WalkDir(agenttest.FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(agenttest.FS, path)
		if err != nil {
			return err
		}
		switch path {
		case agenttest.Version2 + "/manifest.yaml":
			data = bytes.ReplaceAll(data, []byte("services: [web, worker]"), []byte("services: [implicit-web, worker]"))
			data = bytes.ReplaceAll(data, []byte("service: web,"), []byte("service: implicit-web,"))
			data = bytes.ReplaceAll(data, []byte("stop_services: [web, worker]"), []byte("stop_services: [implicit-web, worker]"))
		case agenttest.Version2 + "/compose.yaml.tmpl":
			data = bytes.ReplaceAll(data, []byte("\n  web:"), []byte("\n  implicit-web:"))
			data = bytes.ReplaceAll(data, []byte(`alias "web"`), []byte(`alias "implicit-web"`))
		}
		src[path] = &fstest.MapFile{Data: data}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := packages.NewRegistry(src)
	if err != nil {
		t.Fatal(err)
	}
	e := launchAgent(t, s.project, registry, agentOptions{})
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)
	containers := e.containers(key)
	slices.Sort(containers)
	op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, "none"))
	op = e.waitTerminalOrAwaiting(op.ID)
	routed := packages.ServiceAlias(key, "web", false)
	if op.State == agentapi.OpAwaitingConfirmation {
		out, err := e.edgeVersion(routed)
		if _, aerr := e.client.Abort(t.Context(), op.ID); aerr != nil {
			t.Fatal(aerr)
		}
		e.waitTerminal(op.ID)
		t.Fatalf("the candidate was started; the old route %s answered %q (%v)", routed, out, err)
	}
	e.assertRejectedBeforeStop(key, port, op, containers)
	if got := e.edgeWrite(routed, "acknowledged-through-old-route"); got != "ok" {
		t.Fatalf("write through the old route: %q", got)
	}
	if got := writesLog(t, e, key); !slices.Equal(got, []string{"before-upgrade", "acknowledged-through-old-route"}) {
		t.Fatalf("acknowledged writes changed: %v", got)
	}
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
}

// A deployment this release did not render may carry routed names the
// package rules exclude now. Preflight compares the candidate's names with
// them and refuses the upgrade before anything stops.
func (s *upgradeSuite) testLegacyRouteCollision(t *testing.T, key string, legacy func([]byte, string) []byte) {
	cleanupProject(t, s.project, key)
	e := launchAgent(t, s.project, s.registry, agentOptions{})
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)
	containers := e.containers(key)
	slices.Sort(containers)

	// With the agent stopped, nothing re-renders the deployment before
	// the upgrade reads it; the spec is accepted as if before the stop.
	dir := e.agent.instanceDir(key)
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
	compose, err := os.ReadFile(filepath.Join(dir, composeFile)) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	routed := packages.ServiceAlias(key, "web", false)
	changed := legacy(compose, routed)
	if bytes.Equal(changed, compose) {
		t.Fatalf("the legacy rewrite changed nothing:\n%s", compose)
	}
	if err := writeFileAtomic(filepath.Join(dir, composeFile), changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeSpec(filepath.Join(dir, desiredFile), versionSpec(id, 2, port, agenttest.Version2, "none")); err != nil {
		t.Fatal(err)
	}
	e = launchAgent(t, s.project, s.registry, agentOptions{dataDir: e.dataDir})

	var op agentapi.Operation
	eventually(t, 2*time.Minute, "the upgrade is refused or awaits confirmation", func() bool {
		var ok bool
		op, ok = e.agent.ops.forGeneration(key, 2)
		return ok && (op.Terminal() || op.State == agentapi.OpAwaitingConfirmation)
	})
	if op.State == agentapi.OpAwaitingConfirmation {
		if _, err := e.client.Abort(t.Context(), op.ID); err != nil {
			t.Fatal(err)
		}
		e.waitTerminal(op.ID)
		t.Fatal("a candidate answering to a routed name of the deployment was started")
	}
	e.assertRejectedBeforeStop(key, port, &op, containers)
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
}

// waitTerminalOrAwaiting polls an operation until it ends or awaits its
// confirmation.
func (e *testEnv) waitTerminalOrAwaiting(id uuid.UUID) *agentapi.Operation {
	e.t.Helper()
	var op *agentapi.Operation
	eventually(e.t, 4*time.Minute, "the operation ends or awaits confirmation", func() bool {
		var err error
		op, err = e.client.Operation(e.t.Context(), id)
		return err == nil && (op.Terminal() || op.State == agentapi.OpAwaitingConfirmation)
	})
	return op
}

// edgeWrite writes through a name on the edge network, where Traefik
// would, and returns the answer.
func (e *testEnv) edgeWrite(host, text string) string {
	e.t.Helper()
	proxy := docker(e.t, "ps", "--quiet", "--filter", "label=com.docker.compose.project="+e.project, "--filter", "label=com.docker.compose.service=traefik")
	return docker(e.t, "exec", proxy, "wget", "-q", "-T", "3", "-O", "-", "http://"+host+":8080/cgi-bin/write?"+text)
}

// assertRejectedBeforeStop checks an upgrade refused in preflight: failed
// with policy_violation / alias_not_allowed, no snapshot, the running
// containers untouched and serving, and no decision accepted.
func (e *testEnv) assertRejectedBeforeStop(key string, port int, op *agentapi.Operation, containers []string) {
	e.t.Helper()
	if op.Kind != agentapi.KindUpgrade || op.State != agentapi.OpFailed || op.ErrorCode != agentapi.CodePolicyViolation ||
		op.Rule != packages.RuleAlias || op.SnapshotID != "" {
		e.t.Fatalf("refused upgrade = %+v", op)
	}
	if snapshots, err := e.client.Snapshots(e.t.Context(), key); err != nil || len(snapshots) != 0 {
		e.t.Fatalf("the refused upgrade took a snapshot: %+v %v", snapshots, err)
	}
	current := e.containers(key)
	slices.Sort(current)
	if !slices.Equal(current, containers) {
		e.t.Fatalf("preflight replaced running containers: %v -> %v", containers, current)
	}
	eventually(e.t, 2*time.Minute, "1.0.0-1 serves", func() bool {
		v, err := httpPath(port, "/version")
		return err == nil && v == agenttest.Version+"\n"
	})
	for _, decide := range []func(context.Context, uuid.UUID) (*agentapi.Operation, error){e.client.Confirm, e.client.Abort} {
		var pe *agentclient.ProblemError
		if _, err := decide(e.t.Context(), op.ID); !errors.As(err, &pe) || pe.Code != agentapi.CodeNotAwaitingConfirmation {
			e.t.Fatalf("the refused upgrade accepted a decision: %v", err)
		}
	}
}

// testFencedDNSNames checks both fenced projects with real Docker: the
// candidate awaiting confirmation, and the restored version before it is
// exposed (the agent crashes right after the restored journal entry).
func (s *upgradeSuite) testFencedDNSNames(t *testing.T) {
	const key = "fdns"
	cleanupProject(t, s.project, key)
	e := launchAgent(t, s.project, s.registry, agentOptions{crashAt: agentapi.PhaseRestored})
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)
	op := e.push(key, versionSpec(id, 2, port, agenttest.Version2, "none"))
	op = e.waitAwaiting(op.ID)
	j, err := readJournal(e.agent.instanceDir(key))
	if err != nil || j == nil || !j.RoutedKnown || !slices.Contains(j.Routed, packages.ServiceAlias(key, "web", false)) {
		t.Fatalf("journal routed names: %+v %v", j, err)
	}
	e.assertFencedNames(key, j.Routed)

	if _, err := e.client.Abort(t.Context(), op.ID); err != nil {
		t.Fatal(err)
	}
	eventually(t, 4*time.Minute, "the agent crashes at restored", e.agent.crashed.Load)
	if err := e.stop(); !errors.Is(err, errCrashed) {
		t.Fatalf("agent stopped with %v, want the test crash", err)
	}
	e.assertFencedNames(key, j.Routed)

	e = launchAgent(t, s.project, s.registry, agentOptions{dataDir: e.dataDir})
	if op = e.waitTerminal(op.ID); op.State != agentapi.OpRolledBack || op.ErrorCode != agentapi.CodeAborted {
		t.Fatalf("operation = %+v", op)
	}
	e.assertV1Intact(key, port)
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
}

// testAnchoredSnapshot restores a snapshot whose rendered compose file
// spells the routed alias through a YAML anchor (and, for a version from
// before the suffix reservation, a merge key and a service named
// web-fenced). The restore fences it and the routed alias stays dark until
// the restored version is exposed.
func (s *upgradeSuite) testAnchoredSnapshot(t *testing.T, key string, legacy bool) {
	cleanupProject(t, s.project, key)
	e := launchAgent(t, s.project, s.registry, agentOptions{crashAt: agentapi.PhaseRestored})
	id := uuid.Must(uuid.NewV7())
	port := freePort(t)
	e.installV1(key, id, port)
	op, err := e.client.Snapshot(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	if op = e.waitTerminal(op.ID); op.State != agentapi.OpSucceeded {
		t.Fatalf("snapshot = %+v", op)
	}
	snapID := uuid.MustParse(op.SnapshotID)

	// Rewrite the snapshot's rendered compose file, keeping its checksum
	// valid, as an earlier release could have rendered it.
	renderedDir := filepath.Join(e.agent.snapshotPath(key, snapID), snapshotRendered)
	compose, err := os.ReadFile(filepath.Join(renderedDir, composeFile)) //nolint:gosec // test path
	if err != nil {
		t.Fatal(err)
	}
	routed := packages.ServiceAlias(key, "web", false)
	block := "      edge:\n        aliases: [" + strconv.Quote(routed) + "]\n"
	if !bytes.Contains(compose, []byte(block)) || !bytes.Contains(compose, []byte("\nservices:\n")) {
		t.Fatalf("unexpected rendered compose file:\n%s", compose)
	}
	alias, use := routed, "      edge: *edge\n"
	if legacy {
		alias, use = routed+packages.FencedSuffix, "      edge:\n        <<: *edge\n"
		compose = bytes.Replace(compose, []byte("\n  web:\n"), []byte("\n  web-fenced:\n"), 1)
	}
	compose = bytes.Replace(compose, []byte(block), []byte(use), 1)
	compose = bytes.Replace(compose, []byte("\nservices:\n"), []byte("\nx-edge: &edge\n  aliases: ["+strconv.Quote(alias)+"]\nservices:\n"), 1)
	if err := os.WriteFile(filepath.Join(renderedDir, composeFile), compose, 0o600); err != nil { //nolint:gosec // test path
		t.Fatal(err)
	}
	r, configured, err := readRenderedProject(renderedDir)
	if err != nil {
		t.Fatal(err)
	}
	snap, _, err := e.agent.loadSnapshot(key, snapID)
	if err != nil {
		t.Fatal(err)
	}
	snap.RenderedSHA256 = renderedDigest(r, configured)
	if err := writeJSON(filepath.Join(e.agent.snapshotPath(key, snapID), snapshotMeta), snap); err != nil {
		t.Fatal(err)
	}

	restore, err := e.client.Restore(t.Context(), key, snapID, 2)
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, 4*time.Minute, "the agent crashes at restored", e.agent.crashed.Load)
	if err := e.stop(); !errors.Is(err, errCrashed) {
		t.Fatalf("agent stopped with %v, want the test crash", err)
	}
	j, err := readJournal(e.agent.instanceDir(key))
	if err != nil || j == nil || !j.RoutedKnown {
		t.Fatalf("restore journal: %+v %v", j, err)
	}
	e.assertFencedNames(key, mergeNames(j.Routed, []string{routed, alias}))

	e = launchAgent(t, s.project, s.registry, agentOptions{dataDir: e.dataDir})
	if got := e.waitTerminal(restore.ID); got.State != agentapi.OpSucceeded {
		t.Fatalf("restore = %+v", got)
	}
	eventually(t, 2*time.Minute, "the restored version serves on its routed alias", func() bool {
		v, err := e.edgeVersion(alias)
		return err == nil && v == agenttest.Version
	})
	if err := e.stop(); err != nil {
		t.Fatal(err)
	}
}
