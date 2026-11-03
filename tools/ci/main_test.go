package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/orglane/orglane/tools/ci/internal/gate"
	"github.com/orglane/orglane/tools/ci/internal/risk"
)

func TestRunLicenses(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		input   string
		wantErr bool
		want    string
	}{
		{
			name:  "allowed",
			args:  []string{"licenses", "-format", "pnpm"},
			input: `{"MIT": [{"name": "a", "versions": ["1.0.0"], "license": "MIT"}]}`,
			want:  "1 dependencies OK",
		},
		{
			name:    "copyleft in an Apache package",
			args:    []string{"licenses", "-policy", "apache", "-format", "pnpm"},
			input:   `{"GPL-3.0-only": [{"name": "g", "versions": ["2.0.0"], "license": "GPL-3.0-only"}]}`,
			wantErr: true,
			want:    `g@2.0.0: license "GPL-3.0-only" is not allowed by the apache policy`,
		},
		{
			name:    "unknown Go license",
			args:    []string{"licenses", "-format", "go-licenses"},
			input:   "example.com/x,Unknown,Unknown\n",
			wantErr: true,
			want:    "example.com/x: license is unknown",
		},
		{name: "no Go dependencies", args: []string{"licenses", "-format", "go-licenses"}, want: "0 dependencies OK"},
		{name: "bad format", args: []string{"licenses", "-format", "xml"}, wantErr: true},
		{name: "bad policy", args: []string{"licenses", "-policy", "gpl", "-format", "pnpm"}, wantErr: true},
		{name: "unknown command", args: []string{"deploy"}, wantErr: true},
		{name: "no command", args: nil, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer
			err := run(context.Background(), tt.args, strings.NewReader(tt.input), &out)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v\n%s", err, tt.wantErr, out.String())
			}
			if !strings.Contains(out.String(), tt.want) {
				t.Errorf("output does not contain %q:\n%s", tt.want, out.String())
			}
		})
	}
}

func TestRunRiskWritesOutputs(t *testing.T) {
	dir := t.TempDir()
	output := filepath.Join(dir, "output")
	summary := filepath.Join(dir, "summary")
	// Compare HEAD with itself: no changes, so the tier is low.
	var out bytes.Buffer
	err := run(context.Background(), []string{"risk", "-dir", "../..", "-base", "HEAD", "-head", "HEAD",
		"-github-output", output, "-summary", summary}, nil, &out)
	if err != nil {
		t.Fatalf("risk: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(output) //nolint:gosec // test file in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "tier=low\ncode=false\n" {
		t.Errorf("outputs = %q", got)
	}
	md, err := os.ReadFile(summary) //nolint:gosec // test file in a temp dir
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "`risk/low`") {
		t.Errorf("summary = %q", md)
	}
}

func TestRunPolicyRequiresAPullRequest(t *testing.T) {
	t.Setenv("GITHUB_REPOSITORY", "orglane/orglane")
	t.Setenv("GITHUB_TOKEN", "unused")
	err := run(context.Background(), []string{"policy"}, nil, &bytes.Buffer{})
	if err == nil || errors.Is(err, errFailed) {
		t.Errorf("err = %v, want a usage error", err)
	}
}

func TestParseWorkflowRunEvent(t *testing.T) {
	tests := []struct {
		name        string
		event       string
		numbers     []int
		ownerBranch string
		wantErr     bool
	}{
		{
			name:        "same-repository pull request",
			event:       `{"workflow_run": {"event": "pull_request", "head_branch": "feat/x", "head_repository": {"owner": {"login": "orglane"}}, "pull_requests": [{"number": 18}]}}`,
			numbers:     []int{18},
			ownerBranch: "orglane:feat/x",
		},
		{
			name:        "fork: GitHub leaves pull_requests empty",
			event:       `{"workflow_run": {"event": "pull_request", "head_branch": "main", "head_repository": {"owner": {"login": "someone"}}, "pull_requests": []}}`,
			ownerBranch: "someone:main",
		},
		{
			name:  "push to main",
			event: `{"workflow_run": {"event": "push", "head_branch": "main", "pull_requests": []}}`,
		},
		{name: "not a workflow_run event", event: `{"issue": {"number": 1}}`, wantErr: true},
		{name: "invalid JSON", event: `{`, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			numbers, ownerBranch, err := parseWorkflowRunEvent([]byte(tt.event))
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if !slices.Equal(numbers, tt.numbers) || ownerBranch != tt.ownerBranch {
				t.Errorf("got %v, %q; want %v, %q", numbers, ownerBranch, tt.numbers, tt.ownerBranch)
			}
		})
	}
}

func TestStatusDescription(t *testing.T) {
	res := gate.Evaluate(gate.Input{HeadSHA: "4f2c9d1e8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c3d", ComputedTier: "high"})
	got := statusDescription(res)
	if !strings.HasPrefix(got, "maintainer required (risk/high): ") {
		t.Errorf("statusDescription = %q", got)
	}
	ok := statusDescription(gate.Result{Verdict: gate.Eligible, Tier: "low"})
	if ok != "eligible (risk/low)" {
		t.Errorf("statusDescription = %q", ok)
	}
}

func TestSplitList(t *testing.T) {
	if got := splitList(" lint, test,,build "); !slices.Equal(got, []string{"lint", "test", "build"}) {
		t.Errorf("splitList = %q", got)
	}
}

// TestFetchPull fetches a pull request from a real (local) remote that
// serves refs/pull/N/head like GitHub, and classifies it.
func TestFetchPull(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	work := filepath.Join(root, "work")
	trusted := filepath.Join(root, "trusted")
	git := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper running git
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com",
			"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(work, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	git(root, "init", "-q", "--bare", "-b", "main", remote)
	git(root, "init", "-q", "-b", "main", work)
	write("core/x_test.go", "package x\n\nfunc TestX(t *testing.T) {\n\tt.Fatal(\"bad\")\n}\n")
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "base")
	git(work, "push", "-q", remote, "main")
	// The pull request moves the assertion into a doc.
	git(work, "switch", "-q", "-c", "feature")
	write("core/x_test.go", "package x\n\nfunc TestX(t *testing.T) {\n}\n")
	write("docs/notes.md", "\tt.Fatal(\"bad\")\n")
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "feature")
	git(work, "push", "-q", remote, "feature:refs/pull/7/head")
	// main moves on after the branch point; it must not show up in the diff.
	git(work, "switch", "-q", "main")
	write("README.md", "# x\n")
	git(work, "add", "-A")
	git(work, "commit", "-q", "-m", "main moves")
	git(work, "push", "-q", remote, "main")

	git(root, "clone", "-q", remote, trusted)
	head, base, err := risk.FetchPull(t.Context(), trusted, "origin", 7, "main")
	if err != nil {
		t.Fatal(err)
	}
	changes, err := risk.GitDiff(t.Context(), trusted, base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want the two files of the pull request", changes)
	}
	if res := risk.Classify(changes); res.Tier != risk.High || !res.Code {
		t.Errorf("tier = %s, code = %v; want high, true (reasons %q)", res.Tier, res.Code, res.Reasons)
	}
	// Nothing was checked out: the trusted working tree has no PR files.
	if _, err := os.Stat(filepath.Join(trusted, "docs", "notes.md")); !os.IsNotExist(err) {
		t.Errorf("docs/notes.md exists in the trusted checkout (err = %v)", err)
	}
}
