package packages

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/agentapi"
)

// The names Docker's DNS gives an instance's containers on the shared
// networks (docs/specs/agent.md "Instance names", issue #88).
const (
	nameRouted    = "routed"
	nameFenced    = "fenced"
	nameContainer = "container"
	nameService   = "service"
)

// instanceName is what a DNS name stands for. Bare service names are the
// same for every instance of a package by design (Compose registers them),
// so they carry no key: what must not happen is that two different
// instanceNames produce the same string.
type instanceName struct {
	kind, key, service string
	replica            int
}

func (n instanceName) String() string {
	const project = "orglane"
	switch n.kind {
	case nameRouted:
		return ServiceAlias(n.key, n.service, false)
	case nameFenced:
		return ServiceAlias(n.key, n.service, true)
	case nameContainer:
		return InstanceProject(project, n.key) + "-" + n.service + "-" + strconv.Itoa(n.replica)
	default:
		return n.service
	}
}

// names returns every name of service on the shared networks for key.
func names(key, service string) []instanceName {
	return []instanceName{
		{kind: nameRouted, key: key, service: service},
		{kind: nameFenced, key: key, service: service},
		{kind: nameContainer, key: key, service: service, replica: 1},
		{kind: nameContainer, key: key, service: service, replica: 12},
		{kind: nameService, service: service},
	}
}

// splitAlias inverts ServiceAlias for valid keys and service names: the
// constructive half of the injectivity argument in ValidServiceName.
func splitAlias(alias string) (key, service string, fenced bool) {
	if a, ok := strings.CutSuffix(alias, FencedSuffix); ok {
		alias, fenced = a, true
	}
	i := strings.LastIndexByte(alias, '-')
	return alias[:i], alias[i+1:], fenced
}

// TestServiceNamesOfTheIssue enumerates the colliding combinations of #88:
// before service names lost the "-", each pair of instances produced the
// same routed alias (or container name, or fenced alias). One side of each
// pair is now refused by the manifest and by the policy check.
func TestServiceNamesOfTheIssue(t *testing.T) {
	for _, c := range []struct {
		name          string
		valid         [2]string // key, service: still accepted
		refused       [2]string // key, service: now refused
		sameRouted    bool      // formerly the same routed alias
		sameFenced    bool      // formerly a fenced alias equal to a routed one
		sameContainer bool      // formerly the same container name
	}{
		{"key-with-dash", [2]string{"ab-cd", "e"}, [2]string{"ab", "cd-e"}, true, false, true},
		{"agenttest-like", [2]string{"at1-web", "worker"}, [2]string{"at1", "web-worker"}, true, false, true},
		{"forgejo-like", [2]string{"git-forgejo", "db"}, [2]string{"git", "forgejo-db"}, true, false, true},
		{"digits", [2]string{"a1-b2", "c3"}, [2]string{"a1", "b2-c3"}, true, false, true},
		{"fenced-suffix", [2]string{"ab", "web"}, [2]string{"ab", "web-fenced"}, false, true, false},
		{"fenced-name", [2]string{"ab", "web"}, [2]string{"ab-web", "fenced"}, false, true, false},
		{"dash-end", [2]string{"ab", "web"}, [2]string{"ab", "web-"}, false, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			vk, vs := c.valid[0], c.valid[1]
			rk, rs := c.refused[0], c.refused[1]
			if !agentapi.KeyPattern.MatchString(vk) || !agentapi.KeyPattern.MatchString(rk) {
				t.Fatalf("test keys %q, %q must be valid", vk, rk)
			}
			if !ValidServiceName(vs) {
				t.Fatalf("%q is refused", vs)
			}
			if ValidServiceName(rs) {
				t.Fatalf("%q is accepted", rs)
			}
			if c.sameRouted && ServiceAlias(vk, vs, false) != ServiceAlias(rk, rs, false) {
				t.Fatal("not a former routed collision")
			}
			if c.sameFenced && ServiceAlias(vk, vs, true) != ServiceAlias(rk, rs, false) {
				t.Fatal("not a former fenced collision")
			}
			container := func(k, s string) string {
				return instanceName{kind: nameContainer, key: k, service: s, replica: 1}.String()
			}
			if c.sameContainer && container(vk, vs) != container(rk, rs) {
				t.Fatal("not a former container name collision")
			}

			// Both levels refuse it: the manifest and the policy check.
			base := baseManifest(t)
			if _, err := ParseManifest([]byte(strings.Replace(base, "services: [web, worker]", "services: [web, worker, "+rs+"]", 1))); err == nil {
				t.Fatalf("the manifest accepted the service %q", rs)
			}
			rc := testContext(t)
			rc.Key = rk
			p := normalized(rc)
			svc(p, "web")["networks"].(map[string]any)["edge"] = map[string]any{"aliases": []any{ServiceAlias(rk, "web", false)}}
			if err := checkNormalized(t, p, rc); err != nil {
				t.Fatalf("policy without the service: %v", err)
			}
			p["services"].(map[string]any)[rs] = map[string]any{"image": svc(p, "web")["image"], "restart": "no"}
			var pe *PolicyError
			if err := checkNormalized(t, p, rc); !errors.As(err, &pe) || pe.Rule != RuleAlias || pe.Service != rs {
				t.Fatalf("policy: %v, want %s in %s", err, RuleAlias, rs)
			}
		})
	}
}

// TestInstanceNamesInjective checks every valid key and service name over
// a small alphabet that contains the separator: no two different instance
// names produce the same string, and splitAlias inverts ServiceAlias.
func TestInstanceNamesInjective(t *testing.T) {
	const alphabet = "ab0-"
	var words func(n int) []string
	words = func(n int) []string {
		if n == 0 {
			return []string{""}
		}
		var out []string
		for _, w := range words(n - 1) {
			for _, r := range alphabet {
				out = append(out, w+string(r))
			}
		}
		return out
	}
	var keys, services []string
	for n := 1; n <= 5; n++ {
		for _, w := range words(n) {
			if agentapi.KeyPattern.MatchString(w) {
				keys = append(keys, w)
			}
			if ValidServiceName(w) {
				services = append(services, w)
			}
		}
	}
	// FencedName's own word and a long name, which the alphabet does not
	// produce.
	services = append(services, "fencedx", "x"+strings.Repeat("a", 30))
	keys = append(keys, "fenced", "a-fenced", "x"+strings.Repeat("-a", 11)+"0")
	seen := map[string]instanceName{}
	for _, k := range keys {
		for _, s := range services {
			for _, n := range names(k, s) {
				str := n.String()
				if prev, ok := seen[str]; ok && prev != n {
					t.Fatalf("%v and %v are both %q", prev, n, str)
				}
				seen[str] = n
				if n.kind == nameRouted || n.kind == nameFenced {
					if gk, gs, gf := splitAlias(str); gk != k || gs != s || gf != (n.kind == nameFenced) {
						t.Fatalf("splitAlias(%q) = %q %q %v", str, gk, gs, gf)
					}
					if len(str) > 63 {
						t.Fatalf("%q is longer than a DNS label", str)
					}
				}
			}
		}
	}
	if len(keys) < 100 || len(services) < 50 {
		t.Fatalf("only %d keys and %d services", len(keys), len(services))
	}
}

// FuzzInstanceNames: for any two valid (key, service) pairs, the names of
// different pairs never coincide.
func FuzzInstanceNames(f *testing.F) {
	f.Add("ab-cd", "e", "ab", "cde")
	f.Add("at1", "web", "at1-web", "worker")
	f.Add("ab", "web", "ab-web", "fencedx")
	f.Add("ab-fenced", "web", "ab", "fencedweb")
	f.Fuzz(func(t *testing.T, k1, s1, k2, s2 string) {
		if !agentapi.KeyPattern.MatchString(k1) || !agentapi.KeyPattern.MatchString(k2) || !ValidServiceName(s1) || !ValidServiceName(s2) {
			t.Skip()
		}
		for _, a := range names(k1, s1) {
			for _, b := range names(k2, s2) {
				if a != b && a.String() == b.String() {
					t.Fatalf("%v and %v are both %q", a, b, a.String())
				}
			}
		}
		for _, fenced := range []bool{false, true} {
			if k, s, f := splitAlias(ServiceAlias(k1, s1, fenced)); k != k1 || s != s1 || f != fenced {
				t.Fatalf("splitAlias(ServiceAlias(%q, %q, %v)) = %q %q %v", k1, s1, fenced, k, s, f)
			}
		}
	})
}

func TestRenderRefusesInvalidKey(t *testing.T) {
	p := testPackage(t)
	in, err := p.Inputs(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "a-", "-a", "A", "a_b", "a.b"} {
		rc := testContext(t)
		rc.Key = key
		if _, err := p.Render(testSpec(), in, rc); err == nil {
			t.Errorf("rendered for the invalid key %q", key)
		}
	}
}
