package risk

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func lines(texts ...string) []Line {
	out := make([]Line, len(texts))
	for i, t := range texts {
		out[i] = Line{Number: i + 1, Text: t}
	}
	return out
}

func mod(p string) FileChange { return FileChange{Status: Modified, Path: p, OldPath: p} }
func add(p string) FileChange { return FileChange{Status: Added, Path: p, OldPath: p} }
func del(p string) FileChange { return FileChange{Status: Deleted, Path: p, OldPath: p} }

func withLines(c FileChange, added, removed []Line) FileChange {
	c.Added, c.Removed = added, removed
	return c
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		changes  []FileChange
		want     Tier
		wantCode bool
		reason   string // substring expected in one of the reasons
	}{
		{
			name:    "docs only",
			changes: []FileChange{mod("docs/architecture.md"), add("docs/specs/people.md"), mod("README.md")},
			want:    Low,
			reason:  "all 3 changed files",
		},
		{
			name:    "markdown outside docs",
			changes: []FileChange{mod("core/README.md"), del("docs/old.md")},
			want:    Low,
		},
		{
			name:     "github workflow",
			changes:  []FileChange{mod(".github/workflows/ci.yml")},
			want:     High,
			wantCode: true,
			reason:   "CI and repository configuration",
		},
		{
			name:    "pull request template is still CI configuration",
			changes: []FileChange{mod(".github/pull_request_template.md")},
			want:    High,
		},
		{
			name:     "identity",
			changes:  []FileChange{mod("core/internal/identity/oidc/provider.go"), mod("docs/a.md")},
			want:     High,
			wantCode: true,
			reason:   "identity",
		},
		{
			name:    "ADR",
			changes: []FileChange{add("docs/adr/0019-something.md")},
			want:    High,
			reason:  "architecture decision record",
		},
		{
			name:    "nested license",
			changes: []FileChange{mod("packages/LICENSE")},
			want:    High,
			reason:  "license",
		},
		{name: "trademark", changes: []FileChange{mod("TRADEMARK.md")}, want: High},
		{name: "migration", changes: []FileChange{add("core/migrations/0001_init.sql")}, want: High, wantCode: true},
		{name: "public API", changes: []FileChange{mod("core/api/openapi.yaml")}, want: High, wantCode: true},
		{name: "agent", changes: []FileChange{mod("core/internal/agent/apply.go")}, want: High, wantCode: true},
		{name: "permissions", changes: []FileChange{mod("core/internal/permissions/check.go")}, want: High, wantCode: true},
		{name: "authz", changes: []FileChange{mod("core/internal/authz/permissions.go")}, want: High, wantCode: true},
		{name: "crypto", changes: []FileChange{mod("core/internal/crypto/box.go")}, want: High, wantCode: true},
		{name: "CI tooling", changes: []FileChange{mod("tools/ci/internal/risk/risk.go")}, want: High, wantCode: true},
		{name: "Makefile", changes: []FileChange{mod("Makefile")}, want: High, wantCode: true},
		{
			name:     "release pull request",
			changes:  []FileChange{mod(".release-please-manifest.json"), mod("CHANGELOG.md")},
			want:     High,
			wantCode: true,
			reason:   "cuts a release",
		},
		{name: "release configuration", changes: []FileChange{mod("release-please-config.json")}, want: High, wantCode: true},
		{
			name:     "file moved out of a high-risk directory",
			changes:  []FileChange{{Status: Renamed, Path: "core/internal/people/token.go", OldPath: "core/internal/identity/token.go"}},
			want:     High,
			wantCode: true,
			reason:   "core/internal/identity/token.go",
		},
		{
			name:     "feature code",
			changes:  []FileChange{mod("core/internal/people/service.go"), mod("docs/people.md")},
			want:     Medium,
			wantCode: true,
			reason:   "core/internal/people/service.go",
		},
		{
			name:     "added test files",
			changes:  []FileChange{add("core/internal/people/service_test.go"), add("web/src/app.test.tsx"), add("core/internal/people/testdata/people.csv")},
			want:     Low,
			wantCode: true,
		},
		{
			name: "test file that only gains lines",
			changes: []FileChange{withLines(mod("core/internal/people/service_test.go"),
				lines("func TestMore(t *testing.T) {", `	t.Fatal("x")`, "}"), nil)},
			want:     Low,
			wantCode: true,
		},
		{
			name:     "i18n catalog",
			changes:  []FileChange{mod("packages/i18n/locales/en.json"), add("web/src/locales/de/common.json")},
			want:     Low,
			wantCode: true,
		},
		{
			name:     "deleted i18n catalog",
			changes:  []FileChange{del("packages/i18n/locales/de.json")},
			want:     Medium,
			wantCode: true,
		},
		{
			name:     "deleted Go test file",
			changes:  []FileChange{del("core/internal/people/service_test.go")},
			want:     High,
			wantCode: true,
			reason:   "test file deleted",
		},
		{
			name:     "deleted integration test",
			changes:  []FileChange{del("tests/golden/scenario.go")},
			want:     High,
			wantCode: true,
			reason:   "test file deleted",
		},
		{
			name:     "test renamed to a non-test file",
			changes:  []FileChange{{Status: Renamed, Path: "web/src/app.tsx.bak", OldPath: "web/src/app.test.tsx"}},
			want:     High,
			wantCode: true,
			reason:   "renamed to a non-test file",
		},
		{
			name: "removed Go assertion",
			changes: []FileChange{withLines(mod("core/cmd/orglane/main_test.go"), nil,
				lines(`		t.Fatalf("run() exit code = %d, want 0", code)`))},
			want:     High,
			wantCode: true,
			reason:   "removes an assertion",
		},
		{
			name: "weakened TypeScript assertion",
			changes: []FileChange{withLines(mod("packages/ui-tokens/src/index.test.ts"),
				lines("    expect(true).toBe(true);"), lines("    expect(tokens.color.primary).toBe('#123456');"))},
			want:     High,
			wantCode: true,
			reason:   "removes an assertion",
		},
		{
			name: "Go skip",
			changes: []FileChange{withLines(mod("core/cmd/orglane/main_test.go"),
				lines(`	t.Skip("flaky")`), nil)},
			want:     High,
			wantCode: true,
			reason:   "adds a skip marker",
		},
		{
			name: "Go skipf in a new test file",
			changes: []FileChange{withLines(add("core/x/x_test.go"),
				lines(`	t.Skipf("needs %s", "docker")`), nil)},
			want:     High,
			wantCode: true,
		},
		{name: "it.skip", changes: skipIn("web/src/a.test.ts", `it.skip("works", () => {})`), want: High, wantCode: true},
		{name: "describe.skip", changes: skipIn("web/src/a.test.ts", `describe.skip("suite", () => {})`), want: High, wantCode: true},
		{name: "test.skip", changes: skipIn("web/src/a.test.ts", `test.skip("works", () => {})`), want: High, wantCode: true},
		{name: "generic .skip(", changes: skipIn("web/src/a.spec.ts", `  ctx.skip()`), want: High, wantCode: true},
		{name: "only narrows a suite", changes: skipIn("web/src/a.test.ts", `it.only("works", () => {})`), want: High, wantCode: true},
		{
			name:     "skip marker outside test files is ignored",
			changes:  []FileChange{withLines(mod("web/src/router.ts"), lines(`const skip = list.skip(2)`), nil)},
			want:     Medium,
			wantCode: true,
		},
		{
			name:     "assertion-like line removed from non-test code",
			changes:  []FileChange{withLines(mod("core/internal/people/service.go"), nil, lines(`	assert(x)`))},
			want:     Medium,
			wantCode: true,
		},
		{
			name: "assertion moved into docs is a removal",
			changes: []FileChange{
				withLines(mod("core/a_test.go"), nil, lines(`	t.Fatal("bad")`)),
				withLines(mod("docs/notes.md"), lines(`	t.Fatal("bad")`), nil),
			},
			want:     High,
			wantCode: true,
			reason:   "removes an assertion",
		},
		{
			name: "assertion moved into non-test code is a removal",
			changes: []FileChange{
				withLines(mod("core/a_test.go"), nil, lines(`	t.Fatal("bad")`)),
				withLines(mod("core/a.go"), lines(`	t.Fatal("bad")`), nil),
			},
			want:     High,
			wantCode: true,
			reason:   "removes an assertion",
		},
		{
			name: "assertion moved into a fixture is a removal",
			changes: []FileChange{
				withLines(mod("core/a_test.go"), nil, lines(`	t.Fatal("bad")`)),
				withLines(add("core/testdata/a.txt"), lines(`	t.Fatal("bad")`), nil),
			},
			want:     High,
			wantCode: true,
		},
		{
			name: "commented-out assertion is a removal",
			changes: []FileChange{withLines(mod("core/a_test.go"),
				lines(`	// t.Fatal("bad")`), lines(`	t.Fatal("bad")`))},
			want:     High,
			wantCode: true,
			reason:   "removes an assertion",
		},
		{
			name: "one re-added line cancels only one of two removed duplicates",
			changes: []FileChange{withLines(mod("core/a_test.go"),
				lines(`	t.Fatal("bad")`), lines(`	t.Fatal("bad")`, `		t.Fatal("bad")`))},
			want:     High,
			wantCode: true,
			reason:   "a_test.go:2`: removes an assertion",
		},
		{
			name: "one removed skip marker cancels only one added",
			changes: []FileChange{
				withLines(mod("core/a_test.go"), nil, lines(`	t.Skip("later")`)),
				withLines(mod("core/b_test.go"), lines(`	t.Skip("later")`, `	t.Skip("later")`), nil),
			},
			want:     High,
			wantCode: true,
			reason:   "adds a skip marker",
		},
		{
			name:     "source renamed to docs still changes code",
			changes:  []FileChange{{Status: Renamed, Path: "core/x/source.md", OldPath: "core/x/source.go"}},
			want:     Medium,
			wantCode: true,
		},
		{
			name:     "docs renamed to source changes code",
			changes:  []FileChange{{Status: Renamed, Path: "core/x/source.go", OldPath: "core/x/source.md"}},
			want:     Medium,
			wantCode: true,
		},
		{
			name:    "docs renamed within docs",
			changes: []FileChange{{Status: Renamed, Path: "docs/new.md", OldPath: "docs/old.md"}},
			want:    Low,
		},
		{
			// Without the file contents nothing proves the re-added line is
			// executable, so the removal stands (moves are tested in moves_test.go).
			name: "move without file contents is not trusted",
			changes: []FileChange{
				withLines(mod("core/a_test.go"), nil, lines(`	require.Equal(t, 1, n)`)),
				withLines(add("core/b_test.go"), lines(`	require.Equal(t, 1, n)`), nil),
			},
			want:     High,
			wantCode: true,
		},
		{name: "no changes", changes: nil, want: Low},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Classify(tt.changes)
			if got.Tier != tt.want {
				t.Errorf("tier = %s, want %s (reasons: %q)", got.Tier, tt.want, got.Reasons)
			}
			if got.Code != tt.wantCode {
				t.Errorf("code = %v, want %v", got.Code, tt.wantCode)
			}
			if tt.reason != "" && !strings.Contains(strings.Join(got.Reasons, "\n"), tt.reason) {
				t.Errorf("reasons %q do not mention %q", got.Reasons, tt.reason)
			}
		})
	}
}

func skipIn(p, line string) []FileChange {
	return []FileChange{withLines(mod(p), lines(line), nil)}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		pattern, path string
		want          bool
	}{
		{"**/LICENSE", "LICENSE", true},
		{"**/LICENSE", "packages/LICENSE", true},
		{"**/LICENSE", "packages/LICENSE.md", false},
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", "github/x", false},
		{"core/api/**", "core/api/v1/openapi.yaml", true},
		{"core/api/**", "core/apis/x.go", false},
		{"Makefile", "core/Makefile", false},
	}
	for _, tt := range tests {
		if got := match(tt.pattern, tt.path); got != tt.want {
			t.Errorf("match(%q, %q) = %v, want %v", tt.pattern, tt.path, got, tt.want)
		}
	}
}

func TestParseTier(t *testing.T) {
	for _, s := range []string{"low", "risk/medium", " high "} {
		if _, err := ParseTier(s); err != nil {
			t.Errorf("ParseTier(%q) error: %v", s, err)
		}
	}
	if tier, err := ParseTier("risk/extreme"); err == nil || tier != High {
		t.Errorf("ParseTier(unknown) = %s, %v; want High and an error", tier, err)
	}
}

func TestParsePatch(t *testing.T) {
	patch := `diff --git a/core/a_test.go b/core/a_test.go
index 1111111..2222222 100644
--- a/core/a_test.go
+++ b/core/a_test.go
@@ -10,2 +10,0 @@ func TestA(t *testing.T) {
-	if got != want {
-		t.Fatalf("got %d", got)
@@ -20,0 +19 @@ func TestB(t *testing.T) {
+	t.Skip("later")
diff --git a/old_test.go b/old_test.go
deleted file mode 100644
index 3333333..0000000
--- a/old_test.go
+++ /dev/null
@@ -1 +0,0 @@
-package old
diff --git "a/docs/caf\303\251 notes.md" "b/docs/caf\303\251 notes.md"
--- "a/docs/caf\303\251 notes.md"
+++ "b/docs/caf\303\251 notes.md"
@@ -1 +1 @@
--- a front-matter line
\ No newline at end of file
+++ new front-matter line
`
	got, err := ParsePatch([]byte(patch))
	if err != nil {
		t.Fatal(err)
	}
	a := got["core/a_test.go"]
	if len(a.Removed) != 2 || a.Removed[1].Number != 11 || !strings.Contains(a.Removed[1].Text, "t.Fatalf") {
		t.Errorf("core/a_test.go removed = %+v", a.Removed)
	}
	if len(a.Added) != 1 || a.Added[0].Number != 19 {
		t.Errorf("core/a_test.go added = %+v", a.Added)
	}
	if old := got["old_test.go"]; len(old.Removed) != 1 {
		t.Errorf("old_test.go removed = %+v", old.Removed)
	}
	notes := got["docs/café notes.md"]
	if len(notes.Removed) != 1 || notes.Removed[0].Text != "-- a front-matter line" ||
		len(notes.Added) != 1 || notes.Added[0].Text != "++ new front-matter line" {
		t.Errorf("docs/café notes.md = %+v", notes)
	}
}

func TestParseNameStatus(t *testing.T) {
	out := "M\x00a.go\x00A\x00b_test.go\x00D\x00c.md\x00R087\x00old.go\x00new.go\x00C100\x00x.go\x00y.go\x00T\x00link\x00"
	got, err := ParseNameStatus([]byte(out))
	if err != nil {
		t.Fatal(err)
	}
	want := []FileChange{
		{Status: Modified, Path: "a.go", OldPath: "a.go"},
		{Status: Added, Path: "b_test.go", OldPath: "b_test.go"},
		{Status: Deleted, Path: "c.md", OldPath: "c.md"},
		{Status: Renamed, Path: "new.go", OldPath: "old.go"},
		{Status: Added, Path: "y.go", OldPath: "y.go"},
		{Status: Modified, Path: "link", OldPath: "link"},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d changes, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Status != want[i].Status || got[i].Path != want[i].Path || got[i].OldPath != want[i].OldPath {
			t.Errorf("change %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if _, err := ParseNameStatus([]byte("R100\x00only-old\x00")); err == nil {
		t.Error("truncated rename: want an error")
	}
}

// TestGitDiff runs the classifier end to end against a real git repository.
func TestGitDiff(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatalf("git is required: %v", err)
	}
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // test helper running git with fixed arguments
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
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	commit := func(msg string) {
		t.Helper()
		run("add", "-A")
		run("commit", "-q", "--allow-empty", "-m", msg)
	}

	run("init", "-q", "-b", "main")
	write("docs/guide.md", "# Guide\n")
	write("core/x/x.go", "package x\n\nfunc X() int { return 1 }\n")
	write("core/x/x_test.go", `package x

import "testing"

func TestX(t *testing.T) {
	if X() != 1 {
		t.Fatalf("X() = %d", X())
	}
}
`)
	commit("base")

	steps := []struct {
		name   string
		change func()
		want   Tier
		code   bool
	}{
		{"docs-only change", func() { write("docs/guide.md", "# Guide\n\nMore.\n") }, Low, false},
		{"workflow change", func() { write(".github/workflows/ci.yml", "name: CI\n") }, High, true},
		{"feature change", func() { write("core/x/x.go", "package x\n\nfunc X() int { return 1 + 0 }\n") }, Medium, true},
		{"removed assertion", func() {
			write("core/x/x_test.go", "package x\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {\n\t_ = X()\n}\n")
		}, High, true},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			run("checkout", "-q", "main")
			run("checkout", "-q", "-B", "work")
			s.change()
			commit(s.name)
			changes, err := GitDiff(context.Background(), dir, "main", "work")
			if err != nil {
				t.Fatal(err)
			}
			got := Classify(changes)
			if got.Tier != s.want || got.Code != s.code {
				t.Errorf("tier = %s, code = %v; want %s, %v (reasons: %q)", got.Tier, got.Code, s.want, s.code, got.Reasons)
			}
		})
	}
}

func TestCheckReviewAssets(t *testing.T) {
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), "git", args...) //nolint:gosec // fixed binary and test arguments
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git: %v: %s", err, out)
		}
	}
	run("init", "-q")
	run("commit", "--allow-empty", "-qm", "base")
	for _, path := range []string{".github/pr-assets/web/capture with spaces.png", "docs/screenshots/old.png", "website/public/icon.svg"} {
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("fixture"), 0o600); err != nil {
			t.Fatal(err)
		}
		run("add", path)
		run("commit", "-qm", "asset")
		err := CheckReviewAssets(t.Context(), dir, "HEAD")
		reserved := path != "website/public/icon.svg"
		if (err != nil) != reserved {
			t.Fatalf("%s: %v", path, err)
		}
		// The requested revision, not the working tree, must be checked.
		if err := CheckReviewAssets(t.Context(), dir, "HEAD^"); err != nil {
			t.Fatal(err)
		}
		run("rm", path)
		run("commit", "-qm", "cleanup")
		if err := CheckReviewAssets(t.Context(), dir, "HEAD"); err != nil {
			t.Fatal(err)
		}
	}
	if err := CheckReviewAssets(t.Context(), dir, "missing-revision"); err == nil {
		t.Fatal("invalid revision must fail")
	}
}
