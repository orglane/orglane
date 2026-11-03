package licenses

import (
	"strings"
	"testing"
)

func TestPermits(t *testing.T) {
	tests := []struct {
		expr          string
		server, apach bool
		wantErr       bool
	}{
		{expr: "MIT", server: true, apach: true},
		{expr: "mit", server: true, apach: true},
		{expr: "Apache-2.0", server: true, apach: true},
		{expr: "BSD-2-Clause", server: true, apach: true},
		{expr: "BSD-3-Clause", server: true, apach: true},
		{expr: "ISC", server: true, apach: true},
		{expr: "MPL-2.0", server: true, apach: true},
		{expr: "Python-2.0", server: true, apach: true},
		{expr: "MIT OR Apache-2.0", server: true, apach: true},
		{expr: "(MIT OR GPL-3.0-only)", server: true, apach: true},
		{expr: "MIT AND GPL-3.0-only", server: true, apach: false},
		{expr: "AGPL-3.0-or-later", server: true, apach: false},
		{expr: "AGPL-3.0", server: true, apach: false},
		{expr: "GPL-3.0", server: true, apach: false},
		{expr: "LGPL-2.1", server: true, apach: false},
		{expr: "GPL-2.0+", server: true, apach: false},
		{expr: "GPL-2.0-only", server: false, apach: false},
		{expr: "GPL-2.0", server: false, apach: false},
		{expr: "SSPL-1.0", server: false, apach: false},
		{expr: "BUSL-1.1", server: false, apach: false},
		{expr: "LicenseRef-Proprietary", server: false, apach: false},
		{expr: "Apache-2.0 WITH LLVM-exception", server: true, apach: true},
		{expr: "(MIT AND (BSD-3-Clause OR SSPL-1.0))", server: true, apach: true},
		{expr: "", wantErr: true},
		{expr: "MIT OR", wantErr: true},
		{expr: "(MIT", wantErr: true},
		{expr: "MIT Apache-2.0", wantErr: true},
		{expr: "SEE LICENSE IN LICENSE.txt", wantErr: true},
		{expr: "Apache-2.0 WITH", wantErr: true},
		{expr: "Custom: https://example.com/license", wantErr: true},
	}
	for _, tt := range tests {
		for _, pc := range []struct {
			p    Policy
			want bool
		}{{Server, tt.server}, {Apache, tt.apach}} {
			got, err := pc.p.Permits(tt.expr)
			if (err != nil) != tt.wantErr {
				t.Errorf("%s.Permits(%q) error = %v, wantErr %v", pc.p.Name, tt.expr, err, tt.wantErr)
				continue
			}
			if err == nil && got != pc.want {
				t.Errorf("%s.Permits(%q) = %v, want %v", pc.p.Name, tt.expr, got, pc.want)
			}
		}
	}
}

func TestCheck(t *testing.T) {
	deps := []Dependency{
		{Name: "ok", Version: "1.0.0", License: "MIT"},
		{Name: "copyleft", Version: "2.0.0", License: "GPL-3.0-only"},
		{Name: "missing", Version: "3.0.0", License: ""},
		{Name: "unknown", License: "Unknown"},
		{Name: "custom", Version: "4.0.0", License: "SEE LICENSE IN LICENSE"},
		{Name: "sspl", Version: "5.0.0", License: "SSPL-1.0"},
	}

	server := Check(Server, deps)
	wantServer := []string{
		`custom@4.0.0: license "SEE LICENSE IN LICENSE" is not recognized`,
		"missing@3.0.0: license is unknown",
		`sspl@5.0.0: license "SSPL-1.0" is not allowed by the server policy`,
		"unknown: license is unknown",
	}
	assertViolations(t, server, wantServer)

	apache := Check(Apache, deps)
	if !containsViolation(apache, `copyleft@2.0.0: license "GPL-3.0-only" is not allowed by the apache policy`) {
		t.Errorf("apache policy should reject GPL-3.0-only; got %v", apache)
	}
}

func assertViolations(t *testing.T, got []Violation, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d violations %v, want %d", len(got), got, len(want))
	}
	for i, w := range want {
		if !strings.HasPrefix(got[i].String(), w) {
			t.Errorf("violation %d = %q, want prefix %q", i, got[i], w)
		}
	}
}

func containsViolation(vs []Violation, prefix string) bool {
	for _, v := range vs {
		if strings.HasPrefix(v.String(), prefix) {
			return true
		}
	}
	return false
}

func TestParsePnpm(t *testing.T) {
	in := `{
  "MIT OR Apache-2.0": [{"name": "@biomejs/biome", "versions": ["2.6.4"], "license": "MIT OR Apache-2.0"}],
  "Unknown": [{"name": "mystery", "versions": ["0.1.0", "0.2.0"]}],
  "ISC": [{"name": "isc-pkg", "versions": ["1.0.0"], "license": "ISC"}]
}`
	deps, err := ParsePnpm(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 3 {
		t.Fatalf("got %d deps: %+v", len(deps), deps)
	}
	if deps[2].Name != "mystery" || deps[2].License != "Unknown" || deps[2].Version != "0.1.0, 0.2.0" {
		t.Errorf("mystery = %+v", deps[2])
	}
	if v := Check(Server, deps); len(v) != 1 || v[0].Name != "mystery" {
		t.Errorf("violations = %v, want only mystery", v)
	}

	if deps, err := ParsePnpm(strings.NewReader("{}")); err != nil || len(deps) != 0 {
		t.Errorf("empty object: deps = %v, err = %v", deps, err)
	}
	if deps, err := ParsePnpm(strings.NewReader("")); err != nil || len(deps) != 0 {
		t.Errorf("empty input: deps = %v, err = %v", deps, err)
	}
	if _, err := ParsePnpm(strings.NewReader("not json")); err == nil {
		t.Error("invalid JSON: want an error")
	}
}

func TestParseGoLicenses(t *testing.T) {
	in := "golang.org/x/sys/unix,https://cs.opensource.google/go/x/sys/+/v0.35.0:LICENSE,BSD-3-Clause\n" +
		"github.com/acme/lib,Unknown,Unknown\n" +
		"github.com/acme/gpl,https://github.com/acme/gpl/blob/v1/COPYING,GPL-2.0\n"
	deps, err := ParseGoLicenses(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	v := Check(Server, deps)
	assertViolations(t, v, []string{
		`github.com/acme/gpl: license "GPL-2.0" is not allowed`,
		"github.com/acme/lib: license is unknown",
	})

	if _, err := ParseGoLicenses(strings.NewReader("a,b\n")); err == nil {
		t.Error("two fields: want an error")
	}
}

func TestPolicyByName(t *testing.T) {
	for _, name := range []string{"server", "apache"} {
		p, err := PolicyByName(name)
		if err != nil || p.Name != name || len(p.Allowed()) == 0 {
			t.Errorf("PolicyByName(%q) = %v, %v", name, p.Name, err)
		}
	}
	if _, err := PolicyByName("gpl"); err == nil {
		t.Error("unknown policy: want an error")
	}
}
