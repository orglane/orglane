package packages

import (
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/agentapi"
	forgejo "github.com/orglane/orglane/providers/forgejo/managed"
)

// Every embedded package version loads (the manifest validates and every
// image is pinned by tag and digest, checked by ParseManifest), and the
// upgrade metadata forms paths that reach the newest version.
func TestEmbeddedPackages(t *testing.T) {
	r, err := NewRegistry(sources...)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string][]agentapi.PackageRef{}
	for _, ref := range r.List() {
		byID[ref.ID] = append(byID[ref.ID], ref)
	}
	if len(byID["forgejo"]) == 0 {
		t.Fatal("the forgejo package is not embedded")
	}
	for id, refs := range byID {
		oldest, _ := ParseVersion(refs[0].Version)
		for _, ref := range refs {
			p, _ := r.Get(ref)
			for name, img := range p.Manifest.Images {
				if !strings.Contains(img, "@sha256:") {
					t.Errorf("%s: image %s is not pinned by digest", ref, name)
				}
			}
			// min_from is older than the version (ParseManifest), so the
			// steps cannot form a cycle. It must name an embedded version,
			// or one older than every embedded version (installs that
			// predate them upgrade directly).
			if mf := p.Manifest.Upgrade.MinFrom; mf != "" {
				v, _ := ParseVersion(mf)
				if _, ok := r.Get(agentapi.PackageRef{ID: id, Version: mf}); !ok && v.Compare(oldest) >= 0 {
					t.Errorf("%s: min_from %s is neither embedded nor older than %s", ref, mf, refs[0].Version)
				}
			}
		}
	}
	if newest := byID["forgejo"][len(byID["forgejo"])-1]; newest.Version != forgejo.Current {
		t.Errorf("forgejo: newest embedded version %s, Current says %s", newest.Version, forgejo.Current)
	}
}

func forgejoSpec(mode string) *agentapi.Spec {
	scheme := "https"
	if mode == agentapi.IngressLocal {
		scheme = "http"
	}
	return &agentapi.Spec{
		InstanceID: uuid.MustParse("0190f0f0-0000-7000-8000-000000000002"),
		Generation: 1,
		Package:    agentapi.PackageRef{ID: "forgejo", Version: forgejo.Current},
		State:      agentapi.StateRunning,
		Hosts:      map[string]string{"workspace": "work.acme.test", "web": "git.acme.test"},
		URLs: agentapi.URLs{
			Workspace: scheme + "://work.acme.test", Issuer: scheme + "://work.acme.test",
			Instance: scheme + "://git.acme.test", Webhook: scheme + "://work.acme.test/api/v1/webhooks/x",
		},
		Ingress:  mode,
		Settings: map[string]any{"ssh_port": float64(2222)},
		Secrets: map[string]string{ //nolint:gosec // test secrets
			"db_password": "ZGItcGFzc3dvcmQ", "secret_key": "c2VjcmV0LWtleQ", "admin_password": "YWRtaW4tcGFzc3dvcmQ",
		},
	}
}

type forgejoProject struct {
	Name     string `yaml:"name"`
	Services map[string]struct {
		Image       string            `yaml:"image"`
		User        string            `yaml:"user"`
		ReadOnly    bool              `yaml:"read_only"`
		Environment map[string]string `yaml:"environment"`
		Ports       []string          `yaml:"ports"`
		Networks    map[string]struct {
			Aliases []string `yaml:"aliases"`
		} `yaml:"networks"`
	} `yaml:"services"`
}

func renderForgejo(t *testing.T, spec *agentapi.Spec, rc RenderContext) (*Rendered, forgejoProject) {
	t.Helper()
	r, err := NewRegistry(forgejo.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := r.Get(spec.Package)
	if !ok {
		t.Fatal("forgejo package not found")
	}
	in, err := p.Inputs(spec)
	if err != nil {
		t.Fatal(err)
	}
	out, err := p.Render(spec, in, rc)
	if err != nil {
		t.Fatal(err)
	}
	var project forgejoProject
	if err := yaml.Unmarshal(out.Compose, &project); err != nil {
		t.Fatalf("rendered compose is not YAML: %v\n%s", err, out.Compose)
	}
	return out, project
}

func forgejoContext(t *testing.T, publish string) RenderContext {
	t.Helper()
	rc := testContext(t)
	rc.Key = "git"
	rc.PublishAddress = publish
	return rc
}

func TestRenderForgejo(t *testing.T) {
	out, project := renderForgejo(t, forgejoSpec(agentapi.IngressDomain), forgejoContext(t, "0.0.0.0"))
	f := project.Services["forgejo"]
	if project.Name != "orglane-git" || f.User != "1000:1000" || !f.ReadOnly {
		t.Errorf("project %q, user %q, read_only %v", project.Name, f.User, f.ReadOnly)
	}
	if !slices.Equal(f.Ports, []string{"0.0.0.0:2222:2222"}) {
		t.Errorf("ports = %v", f.Ports)
	}
	if !slices.Equal(f.Networks["edge"].Aliases, []string{"git-forgejo"}) {
		t.Errorf("edge aliases = %v", f.Networks["edge"].Aliases)
	}
	if _, ok := f.Networks["services"]; !ok {
		t.Error("forgejo is not on the services network (PostgreSQL)")
	}
	want := map[string]string{
		"FORGEJO__database__HOST":               "postgres:5432",
		"FORGEJO__database__NAME":               "svc_git",
		"FORGEJO__database__USER":               "svc_git",
		"FORGEJO__database__PASSWD":             "${ORGLANE_SECRET_DB_PASSWORD}",
		"FORGEJO__security__SECRET_KEY":         "${ORGLANE_SECRET_SECRET_KEY}",
		"ORGLANE_ADMIN_PASSWORD":                "${ORGLANE_SECRET_ADMIN_PASSWORD}",
		"FORGEJO__server__DOMAIN":               "git.acme.test",
		"FORGEJO__server__ROOT_URL":             "https://git.acme.test/",
		"FORGEJO__server__SSH_DOMAIN":           "git.acme.test",
		"FORGEJO__server__SSH_PORT":             "2222",
		"FORGEJO__session__COOKIE_SECURE":       "true",
		"FORGEJO__webhook__ALLOWED_HOST_LIST":   "work.acme.test",
		"FORGEJO__service__REQUIRE_SIGNIN_VIEW": "true",
	}
	for k, v := range want {
		if got := f.Environment[k]; got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if f.Environment["FORGEJO__cron_0X2E_update_mirrors__ENABLED"] != "true" || f.Environment["FORGEJO__cron_0X2E_update_checker__ENABLED"] != "false" {
		t.Errorf("cron environment = %v", f.Environment)
	}
	// The global switch would also turn on tasks that are off by default.
	if _, ok := f.Environment["FORGEJO__cron__ENABLED"]; ok {
		t.Error("the global cron switch is set")
	}
	for _, secret := range forgejoSpec(agentapi.IngressDomain).Secrets {
		if strings.Contains(string(out.Compose), secret) {
			t.Error("a secret value is in the compose file")
		}
	}
	if hook, ok := out.Files["configure.sh"]; !ok || hook.Mode != 0o755 {
		t.Error("configure.sh is not rendered executable")
	}

	// Local mode: plain HTTP, ports on loopback, another SSH port.
	local := forgejoSpec(agentapi.IngressLocal)
	local.Settings["ssh_port"] = float64(2422)
	_, project = renderForgejo(t, local, forgejoContext(t, "127.0.0.1"))
	f = project.Services["forgejo"]
	if !slices.Equal(f.Ports, []string{"127.0.0.1:2422:2222"}) {
		t.Errorf("local ports = %v", f.Ports)
	}
	if f.Environment["FORGEJO__server__ROOT_URL"] != "http://git.acme.test/" || f.Environment["FORGEJO__session__COOKIE_SECURE"] != "false" ||
		f.Environment["FORGEJO__server__SSH_PORT"] != "2422" {
		t.Errorf("local environment = %v", f.Environment)
	}

	// Fenced (upgrade candidate): no host port, only the fenced alias, no
	// outbound side effects.
	rc := forgejoContext(t, "0.0.0.0")
	rc.Fenced = true
	_, project = renderForgejo(t, forgejoSpec(agentapi.IngressDomain), rc)
	f = project.Services["forgejo"]
	if len(f.Ports) != 0 || !slices.Equal(f.Networks["edge"].Aliases, []string{"git-forgejo-fenced"}) {
		t.Errorf("fenced: ports %v, aliases %v", f.Ports, f.Networks["edge"].Aliases)
	}
	if f.Environment["FORGEJO__cron_0X2E_update_mirrors__ENABLED"] != "false" || f.Environment["FORGEJO__webhook__ALLOWED_HOST_LIST"] != "orglane-fenced.invalid" {
		t.Errorf("fenced environment = %v", f.Environment)
	}
	// Forgejo keeps a value in app.ini when its variable disappears, so a
	// fenced render must not set anything the unfenced one leaves unset
	// (or the fence outlives the upgrade).
	fencedEnv := f.Environment
	_, project = renderForgejo(t, forgejoSpec(agentapi.IngressDomain), forgejoContext(t, "0.0.0.0"))
	unfencedEnv := project.Services["forgejo"].Environment
	if !slices.Equal(slices.Sorted(maps.Keys(fencedEnv)), slices.Sorted(maps.Keys(unfencedEnv))) {
		t.Errorf("fenced and unfenced renders set different keys:\nfenced   %v\nunfenced %v",
			slices.Sorted(maps.Keys(fencedEnv)), slices.Sorted(maps.Keys(unfencedEnv)))
	}
}

func TestForgejoInputs(t *testing.T) {
	r, err := NewRegistry(forgejo.FS)
	if err != nil {
		t.Fatal(err)
	}
	p, _ := r.Get(agentapi.PackageRef{ID: "forgejo", Version: forgejo.Current})
	spec := forgejoSpec(agentapi.IngressDomain)
	delete(spec.Settings, "ssh_port")
	in, err := p.Inputs(spec)
	if err != nil {
		t.Fatal(err)
	}
	if in.Settings["ssh_port"] != int64(2222) {
		t.Errorf("default ssh_port = %v", in.Settings["ssh_port"])
	}
	for _, bad := range []any{float64(22), float64(70000), "2222"} {
		spec.Settings["ssh_port"] = bad
		if _, err := p.Inputs(spec); err == nil {
			t.Errorf("ssh_port %v accepted", bad)
		}
	}
}
