package risk

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// classifyGit commits base, then applies head (an empty string deletes a
// file) on a branch, and classifies the real git diff between the two.
func classifyGit(t *testing.T, base, head map[string]string) Result {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
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
	apply := func(files map[string]string) {
		t.Helper()
		for name, content := range files {
			p := filepath.Join(dir, name)
			if content == "" {
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		run("add", "-A")
		run("commit", "-q", "--allow-empty", "-m", "change")
	}
	run("init", "-q", "-b", "main")
	apply(base)
	run("checkout", "-q", "-b", "head")
	apply(head)
	changes, err := GitDiff(t.Context(), dir, "main", "head")
	if err != nil {
		t.Fatal(err)
	}
	return Classify(changes)
}

const goTestA = `package a

import "testing"

func TestA(t *testing.T) {
	if got() != 1 {
		t.Fatal("bad")
	}
}
`

// goTestAWithout is goTestA with the assertion removed.
const goTestAWithout = `package a

import "testing"

func TestA(t *testing.T) {
	_ = got()
}
`

const tsTestA = `import { expect, it } from "vitest";

it("works", () => {
  expect(add(1, 2)).toBe(3);
});
`

const tsTestAWithout = `import { it } from "vitest";

it("works", () => {
  add(1, 2);
});
`

type moveCase struct {
	name   string
	base   map[string]string
	head   map[string]string
	want   Tier
	reason string
	// notReason must not appear in the reasons (the counting signal).
	notReason string
}

// round3Cases are the Codex round-3 reproductions and the conservative rules
// for test selection.
func round3Cases() []moveCase {
	const vitestCase = "import { expect, it } from \"vitest\";\n\nit(\"works\", () => {\n  %s\n});\n"
	js := func(body string) string { return strings.Replace(vitestCase, "%s", body, 1) }
	return []moveCase{
		{
			name:   "TS assertion removed from a nested template substitution",
			base:   map[string]string{"web/a.test.ts": js("const result = `${`${expect(1).toBe(1)}`}`;")},
			head:   map[string]string{"web/a.test.ts": js("const result = `${`${1}`}`;")},
			want:   High,
			reason: "removes an assertion",
		},
		{
			name:   "JS assertion removed after a postfix ++ division",
			base:   map[string]string{"web/a.test.js": js("const result = n++ / expect(1).toBe(1) / 2;")},
			head:   map[string]string{"web/a.test.js": js("const result = n++ / 2;")},
			want:   High,
			reason: "removes an assertion",
		},
		{
			name:   "ambiguous syntax in a changed test file",
			base:   map[string]string{"web/a.test.ts": tsTestA},
			head:   map[string]string{"web/a.test.ts": tsTestA + "if (x) {}\n/re/.test(s);\n"},
			want:   High,
			reason: "cannot be analyzed confidently",
		},
		{
			name:   "JSX text the lexer does not support",
			base:   map[string]string{"web/a.test.tsx": tsTestA},
			head:   map[string]string{"web/a.test.tsx": tsTestA + "render(<p>don't</p>);\n"},
			want:   High,
			reason: "cannot be analyzed confidently",
		},
		{
			name:   "Go build constraint added to an existing test file",
			base:   map[string]string{"a/a_test.go": goTestA},
			head:   map[string]string{"a/a_test.go": "//go:build never_run\n\n" + goTestA},
			want:   High,
			reason: "adds a build constraint",
		},
		{
			name:   "legacy +build constraint added",
			base:   map[string]string{"a/a_test.go": goTestA},
			head:   map[string]string{"a/a_test.go": "// +build ignore\n\n" + goTestA},
			want:   High,
			reason: "adds a build constraint",
		},
		{
			name:   "Go build constraint removed",
			base:   map[string]string{"a/a_test.go": "//go:build integration\n\n" + goTestA},
			head:   map[string]string{"a/a_test.go": goTestA},
			want:   High,
			reason: "removes a build constraint",
		},
		{
			name: "new integration test file with the CI tag",
			base: map[string]string{"a/a.go": "package a\n"},
			head: map[string]string{"a/b_test.go": "//go:build integration\n\n" + goTestA},
			want: Low,
		},
		{
			name:   "new test file excluded by an unknown tag",
			base:   map[string]string{"a/a.go": "package a\n"},
			head:   map[string]string{"a/b_test.go": "//go:build never\n\n" + goTestA},
			want:   High,
			reason: "adds a build constraint",
		},
		{
			name:   "test file renamed to a name Go ignores",
			base:   map[string]string{"a/a_test.go": goTestA},
			head:   map[string]string{"a/a_test.go": "", "a/_a_test.go": goTestA},
			want:   High,
			reason: "renamed or moved to `a/_a_test.go`",
		},
		{
			name:   "test file renamed to a platform-specific name",
			base:   map[string]string{"a/a_test.go": goTestA},
			head:   map[string]string{"a/a_test.go": "", "a/a_windows_test.go": goTestA},
			want:   High,
			reason: "renamed or moved",
		},
		{
			name:   "TS test file moved",
			base:   map[string]string{"web/a.test.ts": tsTestA},
			head:   map[string]string{"web/a.test.ts": "", "web/old/a.test.ts": tsTestA},
			want:   High,
			reason: "renamed or moved",
		},
		{
			name:   "vitest configuration changed",
			base:   map[string]string{"web/vitest.config.ts": "export default {};\n"},
			head:   map[string]string{"web/vitest.config.ts": "export default { test: { exclude: [\"**\"] } };\n"},
			want:   High,
			reason: "test discovery configuration",
		},
		{
			name:   "package.json test script changed",
			base:   map[string]string{"web/package.json": "{\n  \"scripts\": {\n    \"test\": \"vitest run\"\n  }\n}\n"},
			head:   map[string]string{"web/package.json": "{\n  \"scripts\": {\n    \"test\": \"true\"\n  }\n}\n"},
			want:   High,
			reason: "changes how tests run",
		},
		{
			name: "package.json dependency changed",
			base: map[string]string{"web/package.json": "{\n  \"scripts\": {\n    \"test\": \"vitest run\"\n  },\n  \"version\": \"1\"\n}\n"},
			head: map[string]string{"web/package.json": "{\n  \"scripts\": {\n    \"test\": \"vitest run\"\n  },\n  \"version\": \"2\"\n}\n"},
			want: Medium,
		},
	}
}

// round4Cases: any change to existing test lines is high (matcher and
// argument changes cannot be judged by counting); pure additions keep the
// normal tier.
func round4Cases() []moveCase {
	const vitestCase = "import { expect, it } from \"vitest\";\n\nit(\"works\", () => {\n%s\n});\n"
	js := func(body string) string { return strings.Replace(vitestCase, "%s", body, 1) }
	const changed = "modifies or deletes"
	return []moveCase{
		{
			name:   "multi-line matcher weakened",
			base:   map[string]string{"web/a.test.ts": js("  expect(result)\n    .toEqual({ id: 123 });")},
			head:   map[string]string{"web/a.test.ts": js("  expect(result)\n    .toBeDefined();")},
			want:   High,
			reason: changed,
		},
		{
			name:   "single-line matcher weakened",
			base:   map[string]string{"web/a.test.ts": js("  expect(result).toEqual({ id: 123 });")},
			head:   map[string]string{"web/a.test.ts": js("  expect(result).toBeTruthy();")},
			want:   High,
			reason: changed,
		},
		{
			name:   "multi-line assertion argument changed",
			base:   map[string]string{"web/a.test.ts": js("  expect(result).toEqual({\n    id: 123,\n  });")},
			head:   map[string]string{"web/a.test.ts": js("  expect(result).toEqual({\n    id: 124,\n  });")},
			want:   High,
			reason: changed,
		},
		{
			name:   "Go assertion condition changed",
			base:   map[string]string{"a/a_test.go": goTestA},
			head:   map[string]string{"a/a_test.go": strings.Replace(goTestA, "got() != 1", "got() < 0", 1)},
			want:   High,
			reason: changed,
		},
		{
			name:   "fixture under testdata modified",
			base:   map[string]string{"a/testdata/golden.json": "{\"id\": 123}\n"},
			head:   map[string]string{"a/testdata/golden.json": "{\"id\": 0}\n"},
			want:   High,
			reason: changed,
		},
		{
			name:   "fixture under __fixtures__ modified",
			base:   map[string]string{"web/__fixtures__/user.json": "{\"id\": 123}\n"},
			head:   map[string]string{"web/__fixtures__/user.json": "{}\n"},
			want:   High,
			reason: changed,
		},
		{
			name: "pure addition to an existing TS test file",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{"web/a.test.ts": tsTestA + "\nit(\"also works\", () => {\n  expect(add(2, 2)).toBe(4);\n});\n"},
			want: Low,
		},
		{
			name: "pure addition to an existing Go test file",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{"a/a_test.go": goTestA + "\nfunc TestB(t *testing.T) {\n\tif got() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"},
			want: Low,
		},
		{
			name: "new fixture file",
			base: map[string]string{"a/a.go": "package a\n"},
			head: map[string]string{"a/testdata/new.json": "{}\n"},
			want: Low,
		},
		{
			name: "pure test addition together with feature code",
			base: map[string]string{"a/a.go": "package a\n\nfunc got() int { return 1 }\n", "a/a_test.go": goTestA},
			head: map[string]string{"a/a.go": "package a\n\nfunc got() int { return 1 }\n\nfunc more() int { return 2 }\n", "a/a_test.go": goTestA + "\nfunc TestMore(t *testing.T) {\n\tif more() != 2 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"},
			want: Medium,
		},
	}
}

func TestAssertionMoves(t *testing.T) {
	tests := []moveCase{
		// Legitimate moves: high because existing test lines change, but the
		// counting signal reports no lost assertion or new skip.
		{
			name: "Go assertion re-indented",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{"a/a_test.go": strings.Replace(goTestA, "\tif got() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}",
				"\tfor range 1 {\n\t\tif got() != 1 {\n\t\t\tt.Fatal( \"bad\" )\n\t\t}\n\t}", 1)},
			want:      High, // existing test lines change: a human checks
			reason:    "modifies or deletes",
			notReason: "assertion",
		},
		{
			name: "Go assertion moved to another test file",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{
				"a/a_test.go": goTestAWithout,
				"a/b_test.go": "package a\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {\n\tif got() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
			},
			want:      High, // existing test lines change: a human checks
			reason:    "modifies or deletes",
			notReason: "assertion",
		},
		{
			name: "TS assertion moved to another test file",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{
				"web/a.test.ts": tsTestAWithout,
				"web/b.test.ts": "import { expect, it } from \"vitest\";\n\nit(\"adds\", () => {\n  expect(add(1, 2)).toBe(3);\n});\n",
			},
			want:      High, // existing test lines change: a human checks
			reason:    "modifies or deletes",
			notReason: "assertion",
		},
		{
			name: "Go assertion moved into the integration tests tree",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{
				"a/a_test.go":                goTestAWithout,
				"tests/integration/check.go": "package integration\n\nfunc check(t T) {\n\tif got() != 1 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
			},
			want:      High, // existing test lines change: a human checks
			reason:    "modifies or deletes",
			notReason: "assertion",
		},
		{
			name: "Go skip marker moved to another test file",
			base: map[string]string{"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n\tt.Skip(\"later\")\n}\n"},
			head: map[string]string{
				"a/a_test.go": "package a\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {\n}\n",
				"a/b_test.go": "package a\n\nimport \"testing\"\n\nfunc TestB(t *testing.T) {\n\tt.Skip(\"later\")\n}\n",
			},
			want:      High, // existing test lines change: a human checks
			reason:    "modifies or deletes",
			notReason: "skip marker",
		},
		{
			name:      "removing a commented-out assertion is harmless",
			base:      map[string]string{"a/a_test.go": "package a\n\nfunc TestA(t *T) {\n\t/*\n\tt.Fatal(\"old\")\n\t*/\n}\n"},
			head:      map[string]string{"a/a_test.go": "package a\n\nfunc TestA(t *T) {\n}\n"},
			want:      High, // existing test lines change: a human checks
			reason:    "modifies or deletes",
			notReason: "assertion",
		},

		// Assertions that only look re-added: they do not execute.
		{
			name: "Go assertion re-added inside a block comment",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{
				"a/a_test.go": goTestAWithout,
				"a/b_test.go": "package a\n/*\n\t\tt.Fatal(\"bad\")\n*/\n",
			},
			want:   High,
			reason: "removes an assertion",
		},
		{
			name: "Go assertions commented out in place with a block comment",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{"a/a_test.go": strings.Replace(goTestA, "\tif got() != 1 {", "\t/*\n\tif got() != 1 {", 1) + "*/\n"},
			want: High,
		},
		{
			name: "Go assertion re-added inside a raw string",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{
				"a/a_test.go": goTestAWithout,
				"a/b_test.go": "package a\n\nvar notes = `\n\t\tt.Fatal(\"bad\")\n`\n",
			},
			want: High,
		},
		{
			name: "Go assertion re-added inside an interpreted string",
			base: map[string]string{"a/a_test.go": "package a\n\nfunc TestA(t *T) {\n\tt.Fatal(\"bad\")\n}\n"},
			head: map[string]string{"a/a_test.go": "package a\n\nfunc TestA(t *T) {\n\tt.Log(\"t.Fatal(\\\"bad\\\")\")\n}\n"},
			want: High,
		},
		{
			name: "Go assertion re-added after a line comment marker",
			base: map[string]string{"a/a_test.go": goTestA},
			head: map[string]string{"a/a_test.go": strings.Replace(goTestA, "\t\tt.Fatal(\"bad\")", "\t\t// t.Fatal(\"bad\")", 1)},
			want: High,
		},
		{
			name: "TS assertion re-added inside a block comment",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{
				"web/a.test.ts": tsTestAWithout,
				"web/b.test.ts": "/*\n  expect(add(1, 2)).toBe(3);\n*/\n",
			},
			want: High,
		},
		{
			name: "TS assertion re-added inside a template literal",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{
				"web/a.test.ts": tsTestAWithout,
				"web/b.test.ts": "export const notes = `\n  expect(add(1, 2)).toBe(3);\n`;\n",
			},
			want: High,
		},
		{
			name: "TS assertion re-added inside a string",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{"web/a.test.ts": strings.Replace(tsTestA, "  expect(add(1, 2)).toBe(3);", "  console.log('expect(add(1, 2)).toBe(3);');", 1)},
			want: High,
		},
		{
			name: "TS assertion commented out with a line comment",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{"web/a.test.ts": strings.Replace(tsTestA, "  expect(", "  // expect(", 1)},
			want: High,
		},
		{
			name:   "TS assertion commented out in place with a block comment",
			base:   map[string]string{"web/a.test.ts": tsTestA},
			head:   map[string]string{"web/a.test.ts": strings.Replace(tsTestA, "  expect(add(1, 2)).toBe(3);\n", "  /*\n  expect(add(1, 2)).toBe(3);\n  */\n", 1)},
			want:   High,
			reason: "no longer executes an assertion",
		},
		{
			name:   "Go skip marker revealed by removing a block comment",
			base:   map[string]string{"a/a_test.go": "package a\n\nfunc TestA(t *T) {\n\t/*\n\tt.Skip(\"later\")\n\t*/\n\tt.Fatal(\"x\")\n}\n"},
			head:   map[string]string{"a/a_test.go": "package a\n\nfunc TestA(t *T) {\n\tt.Skip(\"later\")\n\tt.Fatal(\"x\")\n}\n"},
			want:   High,
			reason: "adds a skip marker",
		},
		{
			name: "TS skip marker added inside a comment still counts",
			base: map[string]string{"web/a.test.ts": tsTestA},
			head: map[string]string{"web/a.test.ts": tsTestA + "// it.skip(\"later\", () => {});\n"},
			want: High,
		},
	}
	tests = append(tests, round3Cases()...)
	tests = append(tests, round4Cases()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyGit(t, tt.base, tt.head)
			if got.Tier != tt.want {
				t.Errorf("tier = %s, want %s (reasons: %q)", got.Tier, tt.want, got.Reasons)
			}
			if tt.reason != "" && !strings.Contains(strings.Join(got.Reasons, "\n"), tt.reason) {
				t.Errorf("reasons %q do not mention %q", got.Reasons, tt.reason)
			}
			if tt.notReason != "" && strings.Contains(strings.Join(got.Reasons, "\n"), tt.notReason) {
				t.Errorf("reasons %q mention %q", got.Reasons, tt.notReason)
			}
		})
	}
}

func TestLexGo(t *testing.T) {
	src := "package a\n" + // 1
		"/* t.Fatal(\"a\")\n" + // 2
		"   t.Fatal(\"b\") */ t.Fatal(\"c\")\n" + // 3
		"var s = `x\n" + // 4
		"t.Fatal(\"d\")\n" + // 5
		"`\n" + // 6
		"func f() { t.Fatal(\"e\") } // t.Error(\"f\")\n" + // 7
		"var q = \"t.Fatal(\\\"g\\\")\"\n" // 8
	lines, ok := lexGo([]byte(src))
	if !ok {
		t.Fatal("lexGo failed")
	}
	want := map[int]lexedLine{
		2: {Code: "", Masked: ""},
		3: {Code: `t.Fatal("c")`, Masked: `t.Fatal("")`},
		4: {Code: "var s=", Masked: "var s="},
		5: {Code: "", Masked: ""},
		7: {Code: `func f(){t.Fatal("e")}`, Masked: `func f(){t.Fatal("")}`},
		8: {Code: `var q="t.Fatal(\"g\")"`, Masked: `var q=""`},
	}
	for n, w := range want {
		if got := lines[n-1]; got != w {
			t.Errorf("line %d = %+v, want %+v", n, got, w)
		}
	}
	if _, ok := lexGo([]byte("package a\nvar s = `unterminated\n")); ok {
		t.Error("unterminated raw string: want ok=false")
	}
}

func TestLexJS(t *testing.T) {
	src := "const a = 1 / 2; // expect(x)\n" + // 1
		"/* expect(a)\n" + // 2
		"   expect(b) */ expect(c);\n" + // 3
		"const re = /\\/\\/ expect(d)/g; expect(e);\n" + // 4
		"const t = `x ${y} expect(f)`;\n" + // 5
		"const m = `\n" + // 6
		"expect(g)\n" + // 7
		"`; const s = 'expect(h)';\n" + // 8
		"return /expect(i)/.test(s);\n" + // 9
		"const n = `${`${expect(j)}`}`;\n" + // 10
		"const q = k++ / expect(l) / 2;\n" + // 11
		"if (ok) /expect(m)/.test(s);\n" + // 12
		"const r = f(x) / expect(n);\n" // 13
	lines, ok := lexJS([]byte(src))
	if !ok {
		t.Fatal("lexJS failed")
	}
	want := map[int]string{ // masked
		1:  "const a=1/2;",
		2:  "",
		3:  "expect(c);",
		4:  "const re=//;expect(e);",
		5:  "const t=`\"\"${y}\"\"`;",
		6:  "const m=`",
		7:  "",
		8:  "`;const s=\"\";",
		9:  "return//.test(s);",
		10: "const n=`${`${expect(j)}`}`;",
		11: "const q=k++/expect(l)/2;",
		12: "if(ok)//.test(s);",
		13: "const r=f(x)/expect(n);",
	}
	for n, w := range want {
		if got := normalize(lines[n-1].Masked); got != w {
			t.Errorf("line %d masked = %q, want %q", n, got, w)
		}
	}
}

func TestLexJSFailsConservatively(t *testing.T) {
	for name, src := range map[string]string{
		"regex or division after a brace": "if (x) {}\n/re/.test(s);\n",
		"unterminated string":             "const s = 'abc;\n",
		"JSX text with an apostrophe":     "render(<p>don't</p>);\n",
		"unterminated template":           "const s = `abc ${x}\n",
		"unterminated block comment":      "/* expect(1)\n",
		"unterminated regex":              "const r = /abc\n",
		"unbalanced parentheses":          "expect(f(1);\n",
		"unbalanced braces":               "it('x', () => {\n",
		"extra closing bracket":           "const a = 1];\n",
	} {
		if _, ok := lexJS([]byte(src)); ok {
			t.Errorf("%s: lexJS succeeded, want a conservative failure", name)
		}
	}
}
