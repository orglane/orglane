package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestEvaluatePaths(t *testing.T) {
	// A tier may be skipped only when every changed path is documentation,
	// the website or a license text. Everything else (the shared core
	// dependencies, test helpers, images, deploy and workspace files the e2e
	// suites use) runs it.
	cases := []struct {
		name   string
		paths  []string
		fullCI bool
		heavy  bool
		web    bool
	}{
		{"no paths", nil, false, true, true},
		{"no paths with the label", nil, true, true, true},
		{"docs", []string{"docs/guide.md", "docs/adr/0020-x.md"}, false, false, false},
		{"docs with the full-ci label", []string{"docs/guide.md"}, true, true, true},
		{"website with the full-ci label", []string{"website/src/pages/index.astro"}, true, true, true},
		{"docs after removing the full-ci label", []string{"docs/guide.md"}, false, false, false},
		{"markdown anywhere", []string{"README.md", "deploy/README.md"}, false, false, false},
		{"website", []string{"website/src/pages/index.astro"}, false, false, false},
		{"license", []string{"LICENSE"}, false, false, false},
		{"mixed docs and code", []string{"docs/guide.md", "core/internal/crypto/password.go"}, false, true, true},
		{"crypto outside identity", []string{"core/internal/crypto/password.go"}, false, true, true},
		{"authz", []string{"core/internal/authz/roles.go"}, false, true, true},
		{"audit", []string{"core/internal/audit/audit.go"}, false, true, true},
		{"test helpers", []string{"tests/pgtest/pgtest.go"}, false, true, true},
		{"shard helper", []string{"tests/shard.sh"}, false, true, true},
		{"dockerignore", []string{".dockerignore"}, false, true, true},
		{"compose", []string{"deploy/compose/compose.yaml"}, false, true, true},
		{"migrations", []string{"core/migrations/0001_init.sql"}, false, true, true},
		{"demo dataset", []string{"demo/people.yaml"}, false, true, true},
		{"workspace config", []string{"pnpm-workspace.yaml"}, false, true, true},
		{"web", []string{"web/src/App.tsx"}, false, true, true},
		{"identity oidc", []string{"core/internal/identity/oidc.go"}, false, true, true},
		{"server", []string{"core/internal/server/server.go"}, false, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			heavy, web := evaluatePaths(c.paths, c.fullCI)
			if heavy != c.heavy || web != c.web {
				t.Fatalf("evaluatePaths(%v, %v) = heavy %v, web %v; want %v, %v", c.paths, c.fullCI, heavy, web, c.heavy, c.web)
			}
		})
	}
}

// TestRunPaths exercises the command the workflow runs, against a real git
// repository, including the full-ci override from a pull request's label.
func TestRunPaths(t *testing.T) {
	dir := newRepo(t, map[string]string{"core/internal/crypto/password.go": "package crypto\n"})
	base := rev(t, dir)
	write(t, dir, "docs/guide.md", "# guide\n")
	commit(t, dir, "docs")
	head := rev(t, dir)

	var out strings.Builder
	if err := runPaths([]string{"-dir", dir, "-base", base, "-head", head}, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "heavy=false\nweb=false\n"; got != want {
		t.Fatalf("runPaths = %q, want %q", got, want)
	}
	out.Reset()
	if err := runPaths([]string{"-dir", dir, "-base", base, "-head", head, "-full-ci"}, &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "heavy=true\nweb=true\n"; got != want {
		t.Fatalf("runPaths with full-ci = %q, want %q", got, want)
	}
}

// TestChangedPathsRenamesAndDeletions: git rename detection is off, so both
// sides of a move are seen. Moving code into an allowlisted path (or docs
// into code) still runs the heavy tiers, and deleting code does too.
func TestChangedPathsRenamesAndDeletions(t *testing.T) {
	cases := []struct {
		name string
		from map[string]string
		move func(t *testing.T, dir string)
		// want lists paths that must be in the diff, if any.
		want  []string
		heavy bool
	}{
		{
			name: "code moved into docs",
			from: map[string]string{"core/internal/apps/coverage_test.go": "package apps\n"},
			move: func(t *testing.T, dir string) {
				mkdir(t, dir, "docs")
				git(t, dir, "mv", "core/internal/apps/coverage_test.go", "docs/coverage.md")
			},
			want:  []string{"core/internal/apps/coverage_test.go", "docs/coverage.md"},
			heavy: true,
		},
		{
			name: "docs moved into code",
			from: map[string]string{"docs/guide.md": "# guide\n"},
			move: func(t *testing.T, dir string) {
				mkdir(t, dir, "core")
				git(t, dir, "mv", "docs/guide.md", "core/guide.go")
			},
			want:  []string{"docs/guide.md", "core/guide.go"},
			heavy: true,
		},
		{
			name: "code deleted",
			from: map[string]string{"core/internal/agent/agent.go": "package agent\n"},
			move: func(t *testing.T, dir string) {
				git(t, dir, "rm", "-q", "core/internal/agent/agent.go")
			},
			want:  []string{"core/internal/agent/agent.go"},
			heavy: true,
		},
		{
			name: "docs renamed and deleted",
			from: map[string]string{"docs/a.md": "# a\n", "docs/b.md": "# b\n"},
			move: func(t *testing.T, dir string) {
				git(t, dir, "mv", "docs/a.md", "docs/c.md")
				git(t, dir, "rm", "-q", "docs/b.md")
			},
			heavy: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := newRepo(t, c.from)
			base := rev(t, dir)
			c.move(t, dir)
			commit(t, dir, "change")
			paths, err := changedPaths(dir, base, rev(t, dir))
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range c.want {
				found := false
				for _, p := range paths {
					if p == want {
						found = true
					}
				}
				if !found {
					t.Fatalf("changedPaths = %v, want %s", paths, want)
				}
			}
			heavy, web := evaluatePaths(paths, false)
			if heavy != c.heavy || web != c.heavy {
				t.Fatalf("tiers for %v = heavy %v, web %v; want %v", paths, heavy, web, c.heavy)
			}
		})
	}
}

func newRepo(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	git(t, dir, "config", "user.email", "ci@example.test")
	git(t, dir, "config", "user.name", "CI")
	for name, content := range files {
		write(t, dir, name, content)
	}
	commit(t, dir, "initial")
	return dir
}

func mkdir(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, name), 0o700); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func commit(t *testing.T, dir, message string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", message)
}

func rev(t *testing.T, dir string) string {
	t.Helper()
	return git(t, dir, "rev-parse", "HEAD")
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...) //nolint:gosec // test helper with fixed arguments
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSelectShard(t *testing.T) {
	names := []string{"TestZ", "TestA", "TestM", "TestB", "TestC", "TestD", "TestE"}
	for shard := 1; shard <= 3; shard++ {
		selected, err := selectShard(names, shardOptions{shard: shard, shards: 3})
		if err != nil {
			t.Fatalf("shard %d: %v", shard, err)
		}
		if len(selected) == 0 {
			t.Fatalf("shard %d is empty", shard)
		}
		if !sorted(selected) {
			t.Fatalf("shard %d is not sorted: %v", shard, selected)
		}
	}
	// Every name lands in exactly one shard, and the selection is stable.
	seen := map[string]int{}
	for shard := 1; shard <= 3; shard++ {
		selected, err := selectShard(names, shardOptions{shard: shard, shards: 3})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range selected {
			seen[name]++
		}
	}
	for _, name := range names {
		if seen[name] != 1 {
			t.Fatalf("%s was selected %d times", name, seen[name])
		}
	}

	// The only filter narrows the set before sharding.
	selected, err := selectShard(names, shardOptions{shard: 1, shards: 1, only: "^(TestA|TestB)$"})
	if err != nil || len(selected) != 2 {
		t.Fatalf("only filter = %v, %v; want TestA and TestB", selected, err)
	}
}

func TestSelectShardNegative(t *testing.T) {
	names := []string{"TestA", "TestB", "TestC"}
	cases := []struct {
		name  string
		opt   shardOptions
		names []string
	}{
		{"no names", shardOptions{shard: 1, shards: 1}, nil},
		{"all skipped", shardOptions{shard: 1, shards: 1, skip: "^Test"}, names},
		{"only matches nothing", shardOptions{shard: 1, shards: 1, only: "^Nope$"}, names},
		{"all selected skipped after only", shardOptions{shard: 1, shards: 1, only: "^TestManifestCapabilities$", skip: "^TestManifestCapabilities$"}, []string{"TestManifestCapabilities"}},
		{"shard zero", shardOptions{shard: 0, shards: 3}, names},
		{"shard above count", shardOptions{shard: 4, shards: 3}, names},
		{"zero shards", shardOptions{shard: 1, shards: 0}, names},
		{"bad only", shardOptions{shard: 1, shards: 1, only: "("}, names},
		{"bad skip", shardOptions{shard: 1, shards: 1, skip: "["}, names},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := selectShard(c.names, c.opt); err == nil {
				t.Fatal("selectShard succeeded, want an error")
			}
		})
	}
}

func needsJSON(results map[string]string) string {
	var b strings.Builder
	b.WriteString("{")
	first := true
	for name, result := range results {
		if !first {
			b.WriteString(",")
		}
		first = false
		fmt.Fprintf(&b, "%q:{\"result\":%q}", name, result)
	}
	b.WriteString("}")
	return b.String()
}

func allSuccess() map[string]string {
	m := map[string]string{}
	for _, job := range aggregateJobs {
		m[job.name] = "success"
	}
	return m
}

func TestEvaluateAggregate(t *testing.T) {
	all := allSuccess()
	if err := evaluateAggregate(needsJSON(all), true, true); err != nil {
		t.Fatalf("all success with every tier selected: %v", err)
	}

	// The tiers not selected are intentionally skipped.
	gated := allSuccess()
	gated["apps-e2e"], gated["agent-e2e"], gated["compose"], gated["web-e2e"] = "skipped", "skipped", "skipped", "skipped"
	if err := evaluateAggregate(needsJSON(gated), false, false); err != nil {
		t.Fatalf("intentional tier skips: %v", err)
	}
	// A heavy job that ran although the tier was not selected is fine.
	gated["apps-e2e"] = "success"
	if err := evaluateAggregate(needsJSON(gated), false, false); err != nil {
		t.Fatalf("unexpected success: %v", err)
	}

	// A skip of a job that is required for this event is not fine.
	for _, job := range []string{"unit", "integration", "apps", "agent", "website", "changes"} {
		results := allSuccess()
		results[job] = "skipped"
		if err := evaluateAggregate(needsJSON(results), true, true); err == nil {
			t.Fatalf("%s skipped with every tier selected: no error", job)
		}
	}
	// A heavy job may not skip when its tier is selected.
	for _, job := range []string{"apps-e2e", "agent-e2e", "compose"} {
		results := allSuccess()
		results[job] = "skipped"
		if err := evaluateAggregate(needsJSON(results), true, true); err == nil {
			t.Fatalf("%s skipped while the heavy tier is selected: no error", job)
		}
	}
	results := allSuccess()
	results["web-e2e"] = "skipped"
	if err := evaluateAggregate(needsJSON(results), true, true); err == nil {
		t.Fatal("web-e2e skipped while the web tier is selected: no error")
	}

	// Failure and cancellation always fail, even for an unselected tier.
	for _, result := range []string{"failure", resultCancelled} {
		results := allSuccess()
		results["apps-e2e"] = result
		if err := evaluateAggregate(needsJSON(results), false, false); err == nil {
			t.Fatalf("apps-e2e %s with the heavy tier unselected: no error", result)
		}
	}

	// Missing jobs and invalid JSON fail closed.
	results = allSuccess()
	delete(results, "unit")
	if err := evaluateAggregate(needsJSON(results), true, true); err == nil {
		t.Fatal("a missing job: no error")
	}
	if err := evaluateAggregate("{", true, true); err == nil {
		t.Fatal("invalid JSON: no error")
	}
}

func TestRunAggregateEnv(t *testing.T) {
	all := allSuccess()
	env := func(values map[string]string) func(string) string {
		return func(k string) string { return values[k] }
	}
	if err := runAggregate(env(map[string]string{"NEEDS": needsJSON(all), "HEAVY": "true", "WEB": "true"}), &strings.Builder{}); err != nil {
		t.Fatalf("all success: %v", err)
	}
	if err := runAggregate(env(map[string]string{"NEEDS": needsJSON(all)}), &strings.Builder{}); err == nil {
		t.Fatal("missing HEAVY/WEB: no error")
	}
	if err := runAggregate(env(map[string]string{"NEEDS": needsJSON(all), "HEAVY": "true", "WEB": "true"}), &strings.Builder{}); err != nil {
		t.Fatalf("valid env: %v", err)
	}
}

func sorted(names []string) bool {
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			return false
		}
	}
	return true
}
