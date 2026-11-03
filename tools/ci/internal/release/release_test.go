package release

import (
	"slices"
	"strings"
	"testing"
)

func TestParseTag(t *testing.T) {
	tests := []struct {
		tag  string
		want string
		pre  bool
		line string
	}{
		{tag: "v0.1.0", want: "0.1.0", line: "0.1"},
		{tag: "v1.20.3", want: "1.20.3", line: "1.20"},
		{tag: "v0.0.0-test.1", want: "0.0.0-test.1", pre: true, line: "0.0"},
		{tag: "v2.0.0-rc.1", want: "2.0.0-rc.1", pre: true, line: "2.0"},
	}
	for _, tt := range tests {
		v, err := ParseTag(tt.tag)
		if err != nil {
			t.Fatalf("ParseTag(%q) error: %v", tt.tag, err)
		}
		if v.String() != tt.want || (v.Prerelease != "") != tt.pre || v.Line() != tt.line {
			t.Errorf("ParseTag(%q) = %q (prerelease %q, line %q), want %q (prerelease %v, line %q)",
				tt.tag, v, v.Prerelease, v.Line(), tt.want, tt.pre, tt.line)
		}
	}
}

func TestParseTagRejectsInvalidTags(t *testing.T) {
	for _, tag := range []string{
		"",
		"v",
		"0.1.0",                              // no v prefix
		"v0.1",                               // not semver
		"v01.2.3",                            // leading zero
		"v1.02.3",                            // leading zero
		"v1.2.3-01",                          // numeric prerelease identifier with a leading zero
		"v1.2.3-",                            // empty prerelease
		"v1.2.3-rc..1",                       // empty identifier
		"v1.2.3+build.5",                     // build metadata is not a valid image tag
		"v1.2.3-rc_1",                        // invalid character
		"vv1.2.3",                            // double prefix
		"v1.2.3\nlatest",                     // injected line
		"refs/tags/v1.2.3",                   // full ref
		"v99999999999999999999.0.0",          // overflows
		"v1.2.3-" + strings.Repeat("a", 130), // longer than an image tag
	} {
		if v, err := ParseTag(tag); err == nil {
			t.Errorf("ParseTag(%q) = %q, want an error", tag, v)
		}
	}
}

func TestAliases(t *testing.T) {
	tests := []struct {
		name string
		tags []string
		want []Alias
	}{
		{name: "empty registry"},
		{name: "only prereleases", tags: []string{"0.0.0-test.1", "0.1.0-rc.1"}},
		{
			name: "first release",
			tags: []string{"0.1.0"},
			want: []Alias{{"latest", "0.1.0"}, {"0.1", "0.1.0"}},
		},
		{
			name: "aliases and other tags are ignored",
			tags: []string{"latest", "0.1", "0.1.0", "edge", "sha256-abc.sig", "v0.9.0", "0.2.0-rc.1"},
			want: []Alias{{"latest", "0.1.0"}, {"0.1", "0.1.0"}},
		},
		{
			name: "patch of an older line keeps latest",
			tags: []string{"0.1.0", "0.2.0", "0.1.5", "0.1.4"},
			want: []Alias{{"latest", "0.2.0"}, {"0.2", "0.2.0"}, {"0.1", "0.1.5"}},
		},
		{
			name: "numeric, not lexical, order",
			tags: []string{"0.9.0", "0.10.0", "0.9.10", "0.9.9"},
			want: []Alias{{"latest", "0.10.0"}, {"0.10", "0.10.0"}, {"0.9", "0.9.10"}},
		},
		{
			name: "prerelease of a newer version does not move latest",
			tags: []string{"1.0.0", "2.0.0-rc.1"},
			want: []Alias{{"latest", "1.0.0"}, {"1.0", "1.0.0"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Aliases(tt.tags); !slices.Equal(got, tt.want) {
				t.Errorf("Aliases(%q) = %v, want %v", tt.tags, got, tt.want)
			}
		})
	}
}

// TestOverlappingReleasesNeverMoveAliasesBackwards simulates the Images
// workflow with releases whose runs overlap. Each release first publishes
// its exact tag (the build job, which runs in parallel across releases) and
// later promotes the aliases (the promote job, serialized in one concurrency
// group). It checks every order of these events, and every subset of skipped
// promotions (GitHub cancels a pending run when another one queues) as long
// as the last promotion runs:
//
//   - an alias never moves to an older version;
//   - in the end latest is the newest release and every line alias the
//     newest patch of its line;
//   - a prerelease never becomes an alias.
func TestOverlappingReleasesNeverMoveAliasesBackwards(t *testing.T) {
	releases := []string{"0.1.0", "0.1.1", "0.2.0", "0.1.2"}
	const prerelease = "0.3.0-rc.1" // published, never promoted
	want := map[string]string{"latest": "0.2.0", "0.2": "0.2.0", "0.1": "0.1.2"}

	type event struct {
		promote bool
		version string
	}
	var events []event
	for _, r := range releases {
		events = append(events, event{false, r}, event{true, r})
	}
	events = append(events, event{false, prerelease})

	sequences := 0
	permute(events, func(seq []event) {
		// The exact tag of a release is published before its promotion.
		pushed := map[string]bool{}
		var promotions []int
		for i, e := range seq {
			if e.promote {
				if !pushed[e.version] {
					return
				}
				promotions = append(promotions, i)
			} else {
				pushed[e.version] = true
			}
		}
		last := promotions[len(promotions)-1]
		for skip := range 1 << (len(promotions) - 1) {
			sequences++
			registry := map[string]string{} // tag -> exact version it points at
			for i, e := range seq {
				if !e.promote {
					registry[e.version] = e.version
					continue
				}
				if n := slices.Index(promotions, i); i != last && skip&(1<<n) != 0 {
					continue // canceled while pending
				}
				tags := make([]string, 0, len(registry))
				for tag := range registry {
					tags = append(tags, tag)
				}
				for _, a := range Aliases(tags) {
					if old, ok := registry[a.Tag]; ok && less(a.Version, old) {
						t.Fatalf("events %v: %s moved back from %s to %s", seq, a.Tag, old, a.Version)
					}
					registry[a.Tag] = a.Version
				}
			}
			for alias, v := range want {
				if registry[alias] != v {
					t.Fatalf("events %v (skipped %b): %s = %q, want %q", seq, skip, alias, registry[alias], v)
				}
			}
			for tag, v := range registry {
				if v == prerelease && tag != prerelease {
					t.Fatalf("events %v: prerelease %s became alias %s", seq, v, tag)
				}
			}
		}
	})
	if sequences == 0 {
		t.Fatal("no event sequences were simulated")
	}
	t.Logf("simulated %d event sequences", sequences)
}

func less(a, b string) bool {
	va, _ := parse(a)
	vb, _ := parse(b)
	return compare(va, vb) < 0
}

// permute calls f with every permutation of s (Heap's algorithm).
func permute[T any](s []T, f func([]T)) {
	s = slices.Clone(s)
	c := make([]int, len(s))
	f(s)
	for i := 0; i < len(s); {
		if c[i] < i {
			if i%2 == 0 {
				s[0], s[i] = s[i], s[0]
			} else {
				s[c[i]], s[i] = s[i], s[c[i]]
			}
			f(s)
			c[i]++
			i = 0
		} else {
			c[i] = 0
			i++
		}
	}
}
