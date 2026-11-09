package main

import (
	"fmt"
	"strings"
	"testing"
)

func TestEvaluatePaths(t *testing.T) {
	// A tier may be skipped only when every changed path is documentation,
	// the website or a license text. Everything else (the shared core
	// dependencies, test helpers, images, deploy and workspace files the e2e
	// suites use) runs it.
	cases := []struct {
		name  string
		paths []string
		heavy bool
		web   bool
	}{
		{"no paths", nil, true, true},
		{"docs", []string{"docs/guide.md", "docs/adr/0020-x.md"}, false, false},
		{"markdown anywhere", []string{"README.md", "deploy/README.md"}, false, false},
		{"website", []string{"website/src/pages/index.astro"}, false, false},
		{"license", []string{"LICENSE"}, false, false},
		{"mixed docs and code", []string{"docs/guide.md", "core/internal/crypto/password.go"}, true, true},
		{"crypto outside identity", []string{"core/internal/crypto/password.go"}, true, true},
		{"authz", []string{"core/internal/authz/roles.go"}, true, true},
		{"audit", []string{"core/internal/audit/audit.go"}, true, true},
		{"test helpers", []string{"tests/pgtest/pgtest.go"}, true, true},
		{"shard helper", []string{"tests/shard.sh"}, true, true},
		{"dockerignore", []string{".dockerignore"}, true, true},
		{"compose", []string{"deploy/compose/compose.yaml"}, true, true},
		{"migrations", []string{"core/migrations/0001_init.sql"}, true, true},
		{"demo dataset", []string{"demo/people.yaml"}, true, true},
		{"workspace config", []string{"pnpm-workspace.yaml"}, true, true},
		{"web", []string{"web/src/App.tsx"}, true, true},
		{"identity oidc", []string{"core/internal/identity/oidc.go"}, true, true},
		{"server", []string{"core/internal/server/server.go"}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			heavy, web := evaluatePaths(c.paths)
			if heavy != c.heavy || web != c.web {
				t.Fatalf("evaluatePaths(%v) = heavy %v, web %v; want %v, %v", c.paths, heavy, web, c.heavy, c.web)
			}
		})
	}
}

func TestRunPaths(t *testing.T) {
	var out strings.Builder
	if err := runPaths(strings.NewReader("core/internal/crypto/password.go\n"), &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "heavy=true\nweb=true\n"; got != want {
		t.Fatalf("runPaths = %q, want %q", got, want)
	}
	out.Reset()
	if err := runPaths(strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "heavy=true\nweb=true\n"; got != want {
		t.Fatalf("empty runPaths = %q, want %q", got, want)
	}
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
