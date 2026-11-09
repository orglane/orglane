// Package risk derives the ADR 0017 risk tier of a change from the files it
// touches and from how it changes test files.
//
// The rules are deliberately conservative: when in doubt, a change is ranked
// higher. Changes to this package are themselves high risk (tools/ci/**).
package risk

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
)

// Tier is an ADR 0017 risk tier.
type Tier int

// Risk tiers, ordered from lowest to highest.
const (
	Low Tier = iota
	Medium
	High
)

// String returns the tier name used in `risk/*` labels.
func (t Tier) String() string {
	switch t {
	case Low:
		return "low"
	case Medium:
		return "medium"
	default:
		return "high"
	}
}

// Label returns the GitHub label for the tier, for example "risk/high".
func (t Tier) Label() string { return "risk/" + t.String() }

// ParseTier parses a tier name ("low", "medium", "high") or label ("risk/low").
func ParseTier(s string) (Tier, error) {
	switch strings.TrimPrefix(strings.TrimSpace(s), "risk/") {
	case "low":
		return Low, nil
	case "medium":
		return Medium, nil
	case "high":
		return High, nil
	}
	return High, fmt.Errorf("unknown risk tier %q", s)
}

// Labels lists every risk label, lowest first.
func Labels() []string { return []string{Low.Label(), Medium.Label(), High.Label()} }

// Result is the classification of a change.
type Result struct {
	Tier Tier
	// Reasons explains the tier: for high, every rule that matched; for
	// medium, the files that are neither docs, added tests nor i18n catalogs.
	Reasons []string
	// Code is false when the change only touches documentation and license
	// texts, so lint, tests and builds can be skipped.
	Code bool
}

// highPaths are path rules that always make a change high risk (ADR 0017:
// CI configuration, releases, licenses, ADRs, identity, permissions, cryptography,
// migrations, the agent, public API contracts). Keep in sync with
// .github/CODEOWNERS.
var highPaths = []struct {
	pattern string
	why     string
}{
	{".github/**", "CI and repository configuration"},
	{"tools/ci/**", "CI tooling (risk labeler, merge checklist, license check)"},
	{"Makefile", "CI gate definitions (make lint/test/build)"},
	{".golangci.yml", "lint configuration"},
	{"biome.json", "lint configuration"},
	{"release-please-config.json", "release configuration (releases are a human decision)"},
	{".release-please-manifest.json", "release version (merging the release pull request cuts a release)"},
	{"**/LICENSE", "license"},
	{"TRADEMARK.md", "trademark policy"},
	{"docs/adr/**", "architecture decision record"},
	{"core/internal/identity/**", "identity and authentication"},
	{"core/internal/permissions/**", "permissions"},
	{"core/internal/authz/**", "permissions"},
	{"core/internal/crypto/**", "cryptography"},
	{"core/migrations/**", "database schema migration"},
	{"core/internal/agent/**", "privileged agent"},
	{"core/api/**", "public API contract"},
	{"**/vitest.config.*", "test discovery configuration"},
	{"**/vitest.workspace.*", "test discovery configuration"},
	{"**/vite.config.*", "build and test configuration"},
}

var (
	// skipMarker matches lines that skip or narrow down tests.
	skipMarker = regexp.MustCompile(`\.Skip(Now|f)?\(|\.skip(If)?\(|\b(it|describe|test|suite|bench)\.(skip|todo|only)\b|\.only\(|\b(xit|xdescribe|xtest)\(`)
	// assertion matches lines that assert something in Go or TypeScript tests.
	assertion = regexp.MustCompile(`\b(t|b|f|tb)\.(Error|Errorf|Fatal|Fatalf|Fail|FailNow)\(|\b(assert|require)\.[A-Z]\w*\(|\bexpect(\.\w+)?\(|\bassert(\.\w+)?\(|\.should\b`)
)

// Classify returns the risk tier of a set of changes.
func Classify(changes []FileChange) Result {
	var res Result
	var high, medium []string

	for _, c := range changes {
		paths := []string{c.Path}
		if c.OldPath != "" && c.OldPath != c.Path {
			paths = append(paths, c.OldPath)
		}

		// A rename between code and docs still changes code (both sides count).
		if slices.ContainsFunc(paths, func(p string) bool { return !isDocs(p) }) {
			res.Code = true
		}
		for _, p := range paths {
			if why, ok := highPath(p); ok {
				high = append(high, fmt.Sprintf("`%s`: %s", p, why))
			}
		}
		high = append(high, testConfigChanges(c)...)

		if !lowFile(c) {
			medium = append(medium, fmt.Sprintf("`%s`: not docs, an added test or an i18n catalog", c.Path))
		}
	}
	high = append(high, testWeakening(changes)...)

	switch {
	case len(high) > 0:
		res.Tier, res.Reasons = High, high
	case len(medium) > 0:
		res.Tier, res.Reasons = Medium, medium
	default:
		res.Tier = Low
		res.Reasons = []string{fmt.Sprintf("all %d changed files are docs, added tests or i18n catalogs", len(changes))}
	}
	return res
}

// testWeakening reports deleted test files, added skip markers and removed
// assertions.
//
// Assertions are counted, not diffed: across all changed test code, every
// executable assertion of the old files must still execute in the new files
// (same code, ignoring whitespace; moved or re-indented is fine). Whether code
// executes is decided by lexing the whole file (go/scanner for Go, a small
// tokenizer for TypeScript/JavaScript), so assertions that end up inside a
// comment (even an in-place block comment that leaves the assertion lines
// untouched), a multi-line string or a template literal, or that only appear
// inside a string, no longer count. Docs, fixtures and non-test code never
// count. Executable skip markers are counted the other way round: the new
// files must not have more. When a side of a file is unknown or cannot be
// lexed, its diff lines are judged conservatively (removed assertions and
// added skip markers are reported, and nothing offsets them).
func testWeakening(changes []FileChange) []string {
	var out []string
	oldAsserts, newAsserts := map[string][]site{}, map[string]int{}
	oldSkips := map[string]int{}
	var newSkips []site

	for _, c := range changes {
		switch {
		case c.Status == Deleted && isTest(c.Path):
			// Every assertion of a deleted file is gone; one reason is enough.
			out = append(out, fmt.Sprintf("`%s`: test file deleted", c.Path))
			continue
		case c.Status == Renamed && isTest(c.OldPath) && !isTest(c.Path):
			out = append(out, fmt.Sprintf("`%s`: test file renamed to a non-test file `%s`", c.OldPath, c.Path))
		case c.Status == Renamed && isTest(c.OldPath):
			// The new name may exclude the file from test runs (_x_test.go,
			// x_windows_test.go, a path outside the runner's include list).
			out = append(out, fmt.Sprintf("`%s`: test file renamed or moved to `%s`", c.OldPath, c.Path))
		}
		out = append(out, buildConstraintChanges(c)...)
		// Matcher and argument changes (toEqual(x) -> toBeDefined()) cannot be
		// judged by counting, so any change to existing test lines needs a
		// human. Pure additions keep the normal tier.
		if isTest(c.OldPath) && c.Status != Added && len(c.Removed) > 0 {
			out = append(out, fmt.Sprintf("`%s`: modifies or deletes %d existing test line(s); changes to existing tests need a maintainer", c.OldPath, len(c.Removed)))
		}

		removed := lineSet(c.Removed)
		if isTest(c.OldPath) && c.Status != Added {
			old, failed := lexSide(c.OldPath, c.OldContent)
			if failed {
				out = append(out, fmt.Sprintf("`%s`: cannot be analyzed confidently (unsupported or ambiguous syntax); a human must check the test change", c.OldPath))
			}
			if old != nil {
				for i, ll := range old.lines {
					if assertion.MatchString(ll.Masked) {
						key := normalize(ll.Code)
						oldAsserts[key] = append(oldAsserts[key], site{path: c.OldPath, line: i + 1, inDiff: removed[i+1], key: key})
					}
					if skipMarker.MatchString(ll.Masked) {
						oldSkips[normalize(ll.Code)]++
					}
				}
			} else {
				for _, l := range c.Removed {
					if assertion.MatchString(l.Text) {
						out = append(out, fmt.Sprintf("`%s:%d`: removes an assertion: `%s`", c.OldPath, l.Number, strings.TrimSpace(l.Text)))
					}
				}
			}
		}

		if isTest(c.Path) && c.Status != Deleted {
			added := lineSet(c.Added)
			nw, failed := lexSide(c.Path, c.NewContent)
			if failed {
				out = append(out, fmt.Sprintf("`%s`: cannot be analyzed confidently (unsupported or ambiguous syntax); a human must check the test change", c.Path))
			}
			if nw != nil {
				for i, ll := range nw.lines {
					if assertion.MatchString(ll.Masked) {
						newAsserts[normalize(ll.Code)]++
					}
					if skipMarker.MatchString(ll.Masked) {
						newSkips = append(newSkips, site{path: c.Path, line: i + 1, inDiff: added[i+1], key: normalize(ll.Code)})
					}
				}
			}
			// Conservative: an added skip marker that does not execute (in a
			// comment or string) or cannot be lexed still counts.
			for _, l := range c.Added {
				if skipMarker.MatchString(l.Text) {
					if ll, ok := nw.line(l); !ok || !skipMarker.MatchString(ll.Masked) {
						out = append(out, fmt.Sprintf("`%s:%d`: adds a skip marker: `%s`", c.Path, l.Number, strings.TrimSpace(l.Text)))
					}
				}
			}
		}
	}

	// Assertions that execute fewer times than before.
	keys := make([]string, 0, len(oldAsserts))
	for k := range oldAsserts {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		sites := oldAsserts[k]
		missing := len(sites) - newAsserts[k]
		if missing <= 0 {
			continue
		}
		// Prefer the occurrences the diff removed; the rest were disabled
		// in place (for example by a surrounding block comment).
		slices.SortStableFunc(sites, func(a, b site) int {
			switch {
			case (a.inDiff != "") == (b.inDiff != ""):
				return 0
			case a.inDiff != "":
				return -1
			}
			return 1
		})
		for _, st := range sites[:missing] {
			what := "no longer executes an assertion"
			if st.inDiff != "" {
				what = "removes an assertion"
			}
			text := st.inDiff
			if text == "" {
				text = k
			}
			out = append(out, fmt.Sprintf("`%s:%d`: %s: `%s`", st.path, st.line, what, strings.TrimSpace(text)))
		}
	}

	// Skip markers that execute more often than before.
	for _, st := range newSkips {
		if oldSkips[st.key] > 0 {
			oldSkips[st.key]--
			continue
		}
		text := st.inDiff
		if text == "" {
			text = st.key
		}
		out = append(out, fmt.Sprintf("`%s:%d`: adds a skip marker: `%s`", st.path, st.line, strings.TrimSpace(text)))
	}
	return out
}

var buildConstraint = regexp.MustCompile(`^//\s*(go:build|\+build)\b`)

// buildConstraintChanges reports changed Go build constraints in test files:
// they decide whether the tests compile and run at all. A new test file may
// use the "integration" tag that CI runs.
func buildConstraintChanges(c FileChange) []string {
	if !isTest(c.Path) && !isTest(c.OldPath) {
		return nil
	}
	if path.Ext(c.Path) != ".go" && path.Ext(c.OldPath) != ".go" {
		return nil
	}
	var out []string
	report := func(p string, l Line, verb string) {
		out = append(out, fmt.Sprintf("`%s:%d`: %s a build constraint: `%s`", p, l.Number, verb, strings.TrimSpace(l.Text)))
	}
	for _, l := range c.Added {
		t := strings.TrimSpace(l.Text)
		if !buildConstraint.MatchString(t) {
			continue
		}
		if c.Status == Added && t == "//go:build integration" {
			continue
		}
		report(c.Path, l, "adds")
	}
	for _, l := range c.Removed {
		if buildConstraint.MatchString(strings.TrimSpace(l.Text)) {
			report(c.OldPath, l, "removes")
		}
	}
	return out
}

// testCommand matches package.json keys that define how tests run.
var testCommand = regexp.MustCompile(`"(test|vitest)[\w:.-]*"\s*:`)

// testConfigChanges reports changes to the test command in package.json.
func testConfigChanges(c FileChange) []string {
	if path.Base(c.Path) != "package.json" && path.Base(c.OldPath) != "package.json" {
		return nil
	}
	var out []string
	for _, l := range append(slices.Clone(c.Added), c.Removed...) {
		if testCommand.MatchString(l.Text) {
			out = append(out, fmt.Sprintf("`%s:%d`: changes how tests run: `%s`", c.Path, l.Number, strings.TrimSpace(l.Text)))
		}
	}
	return out
}

// site is an occurrence of an assertion or skip marker. inDiff is the line's
// text when the diff added or removed it.
type site struct {
	path   string
	line   int
	inDiff string
	key    string
}

func lineSet(lines []Line) map[int]string {
	m := make(map[int]string, len(lines))
	for _, l := range lines {
		m[l.Number] = l.Text
	}
	return m
}

// lexed is one side (old or new) of a changed file, split into executable
// code per line. A nil *lexed means the content is unknown or not lexable.
type lexed struct{ lines []lexedLine }

// lexSide lexes one side of a changed file. failed is true when the file is
// test code with known content that could not be lexed confidently.
func lexSide(name string, content []byte) (x *lexed, failed bool) {
	if content == nil || !isTestCode(name) {
		return nil, false
	}
	lines, ok := lexFile(name, content)
	if !ok {
		return nil, true
	}
	return &lexed{lines: lines}, false
}

func (x *lexed) line(l Line) (lexedLine, bool) {
	if x == nil || l.Number < 1 || l.Number > len(x.lines) {
		return lexedLine{}, false
	}
	return x.lines[l.Number-1], true
}

func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }

// lowFile reports whether a change on its own would be low risk: docs, an
// added test (a new test file, or a test file that only gains lines) or an
// i18n catalog.
func lowFile(c FileChange) bool {
	if isDocs(c.Path) && (c.OldPath == "" || isDocs(c.OldPath)) {
		return true
	}
	if isTest(c.Path) {
		switch c.Status {
		case Added:
			return true
		case Modified:
			return len(c.Removed) == 0
		}
	}
	return isI18nCatalog(c.Path) && c.Status != Deleted
}

func highPath(p string) (string, bool) {
	for _, r := range highPaths {
		if match(r.pattern, p) {
			return r.why, true
		}
	}
	return "", false
}

// isDocs reports whether p is documentation: anything under docs/ (ADRs are
// docs too, but also high risk) and Markdown files anywhere.
func isDocs(p string) bool {
	return strings.HasPrefix(p, "docs/") || strings.EqualFold(path.Ext(p), ".md") || path.Base(p) == "LICENSE"
}

var codeExt = regexp.MustCompile(`\.(go|[cm]?[jt]sx?)$`)

// isTestCode reports whether p is executable test code (not docs or
// fixtures): a test file by name, or a Go/TS/JS file under tests/.
func isTestCode(p string) bool {
	return isTest(p) && codeExt.MatchString(p) && !slices.Contains(strings.Split(path.Dir(p), "/"), "testdata")
}

var testFile = regexp.MustCompile(`(_test\.go|\.(test|spec)\.[cm]?[jt]sx?)$`)

// isTest reports whether p is a test file or test data: Go and TypeScript
// unit tests, the tests/ tree (integration, end-to-end and the golden
// scenario) and testdata/ fixtures.
func isTest(p string) bool {
	if p == "" {
		return false
	}
	if testFile.MatchString(path.Base(p)) || strings.HasPrefix(p, "tests/") {
		return true
	}
	return slices.ContainsFunc(strings.Split(path.Dir(p), "/"), func(s string) bool {
		return s == "testdata" || s == "__tests__" || s == "__fixtures__"
	})
}

// isI18nCatalog reports whether p is a translation catalog: a JSON, YAML or
// gettext file inside a locales/ directory.
func isI18nCatalog(p string) bool {
	if !slices.Contains(strings.Split(path.Dir(p), "/"), "locales") {
		return false
	}
	switch path.Ext(p) {
	case ".json", ".yaml", ".yml", ".po":
		return true
	}
	return false
}

// match reports whether p matches a glob pattern where "**" matches any
// number of path segments and other segments use path.Match syntax.
func match(pattern, p string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchSegments(pattern, segs []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			rest := pattern[1:]
			for i := 0; i <= len(segs); i++ {
				if matchSegments(rest, segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], segs[0]); err != nil || !ok {
			return false
		}
		pattern, segs = pattern[1:], segs[1:]
	}
	return len(segs) == 0
}
