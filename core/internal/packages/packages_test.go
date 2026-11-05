package packages

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/agentapi"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

func testRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func testPackage(t *testing.T) *Package {
	t.Helper()
	p, ok := testRegistry(t).Get(agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version})
	if !ok {
		t.Fatal("agenttest package not found")
	}
	return p
}

// testSpec is a valid spec for the agenttest package.
func testSpec() *agentapi.Spec {
	return &agentapi.Spec{
		InstanceID: uuid.MustParse("0190f0f0-0000-7000-8000-000000000001"),
		Generation: 1,
		Package:    agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version},
		State:      agentapi.StateRunning,
		Hosts:      map[string]string{"workspace": "orglane.localhost", "web": "agenttest.orglane.localhost"},
		URLs: agentapi.URLs{
			Workspace: "http://orglane.localhost", Issuer: "http://orglane.localhost",
			Instance: "http://agenttest.orglane.localhost", Webhook: "http://orglane.localhost/api/v1/webhooks/x",
		},
		Ingress:  agentapi.IngressLocal,
		Settings: map[string]any{"http_port": float64(18081), "message": "hi"},
		Secrets:  map[string]string{"db_password": "c2VjcmV0LXBhc3N3b3Jk"}, //nolint:gosec // a test secret
	}
}

func testContext(t *testing.T) RenderContext {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "instances", "at1")
	return RenderContext{
		Key: "at1", InstanceDir: dir, ComposeProject: "orglane", PublishAddress: "127.0.0.1",
		EdgeNetwork: "orglane_edge", ServicesNetwork: "orglane_services", DatabaseHost: "postgres",
		MemoryBytes: 1 << 30, CPUs: 2,
	}
}

func TestRegistryLoadsAgenttest(t *testing.T) {
	got := testRegistry(t).List()
	want := []agentapi.PackageRef{{ID: "agenttest", Version: "1.0.0-1"}, {ID: "agenttest", Version: "2.0.0-1"}, {ID: "agenttest", Version: "3.0.0-1"}}
	if !slices.Equal(got, want) {
		t.Fatalf("List() = %v, want %v", got, want)
	}
	if _, ok := testRegistry(t).Get(agentapi.PackageRef{ID: "agenttest", Version: "9.9.9-1"}); ok {
		t.Fatal("Get found a version that is not embedded")
	}
}

func TestVersion(t *testing.T) {
	for _, bad := range []string{"", "1.0.0", "1.0-1", "01.0.0-1", "1.0.0-0", "1.0.0-1-2", "v1.0.0-1", "1.0.0-rc1"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q) accepted", bad)
		}
	}
	order := []string{"1.0.0-1", "1.0.0-2", "1.0.10-1", "1.2.0-1", "12.0.0-1", "13.0.2-1"}
	for i := 1; i < len(order); i++ {
		a, _ := ParseVersion(order[i-1])
		b, err := ParseVersion(order[i])
		if err != nil {
			t.Fatal(err)
		}
		if a.Compare(b) >= 0 || b.Compare(a) <= 0 || b.String() != order[i] {
			t.Errorf("%s should sort before %s", a, b)
		}
	}
}

func baseManifest(t *testing.T) string {
	t.Helper()
	b, err := agenttest.FS.ReadFile(agenttest.Version + "/manifest.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestManifestValidation(t *testing.T) {
	base := baseManifest(t)
	if _, err := ParseManifest([]byte(base)); err != nil {
		t.Fatalf("agenttest manifest: %v", err)
	}
	cases := map[string]struct{ old, new string }{
		"unknown field":         {"provider: agenttest", "provider: agenttest\nprivileged: true"},
		"unpinned image":        {"busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec", "busybox:1.37.0"},
		"implicit registry":     {"docker.io/library/busybox", "busybox"},
		"reserved service":      {"services: [web, worker]", "services: [web, worker, postgres]"},
		"fence suffix service":  {"services: [web, worker]", "services: [web, worker, web-fenced]"},
		"fence name service":    {"services: [web, worker]", "services: [web, worker, fenced]"},
		"bad version":           {"version: 1.0.0-1", "version: 1.0.0"},
		"wrong format":          {"format: 1", "format: 2"},
		"database password":     {"secrets: [db_password]", "secrets: [other]"},
		"port setting":          {"setting: http_port", "setting: message"},
		"route service":         {"service: web, port: 8080", "service: nope, port: 8080"},
		"hook service":          {"{ service: worker, command", "{ service: nope, command"},
		"hook timeout":          {"timeout: 1m }", "timeout: 0s }"},
		"data dir mode":         {`mode: "0755"`, `mode: "rwx"`},
		"capability":            {"capabilities_add: []", "capabilities_add: [ALL]"},
		"min_from not older":    {"confirm_timeout: 2m", "confirm_timeout: 2m\n  min_from: 1.0.0-1"},
		"default out of range":  {"max: 65535, default: 18080", "max: 65535, default: 80"},
		"string pattern syntax": {"type: string, default: hello", "type: string, pattern: '(', default: hello"},
		"start timeout":         {"start_timeout: 2m", "start_timeout: 0s"},
		"backup data dir":       {"data_dirs: [www]", "data_dirs: [nope]"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(base, c.old) {
				t.Fatalf("fragment %q not in manifest", c.old)
			}
			if _, err := ParseManifest([]byte(strings.Replace(base, c.old, c.new, 1))); err == nil {
				t.Fatal("manifest accepted")
			}
		})
	}
}

func TestRegistryRejectsMismatchedDirectory(t *testing.T) {
	src := fstest.MapFS{}
	for _, f := range []string{"manifest.yaml", "compose.yaml.tmpl"} {
		b, err := agenttest.FS.ReadFile(agenttest.Version + "/" + f)
		if err != nil {
			t.Fatal(err)
		}
		src["2.0.0-1/"+f] = &fstest.MapFile{Data: b}
	}
	if _, err := NewRegistry(src); err == nil {
		t.Fatal("a directory holding another version was accepted")
	}
}

func TestInputs(t *testing.T) {
	p := testPackage(t)
	in, err := p.Inputs(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	if in.Settings["http_port"] != int64(18081) || in.Settings["message"] != "hi" {
		t.Fatalf("settings = %v", in.Settings)
	}
	// Defaults apply.
	spec := testSpec()
	spec.Settings = nil
	if in, err = p.Inputs(spec); err != nil || in.Settings["http_port"] != int64(18080) || in.Settings["message"] != "hello" {
		t.Fatalf("defaults: %v, %v", in.Settings, err)
	}

	const secretValue = "it's-$ecret\n"
	cases := map[string]struct {
		mutate func(*agentapi.Spec)
		field  string
	}{
		"port below min":      {func(s *agentapi.Spec) { s.Settings["http_port"] = float64(80) }, "settings.http_port"},
		"port not integer":    {func(s *agentapi.Spec) { s.Settings["http_port"] = 18080.5 }, "settings.http_port"},
		"port as string":      {func(s *agentapi.Spec) { s.Settings["http_port"] = "18080" }, "settings.http_port"},
		"undeclared setting":  {func(s *agentapi.Spec) { s.Settings["image"] = "evil:latest" }, "settings.image"},
		"newline in message":  {func(s *agentapi.Spec) { s.Settings["message"] = "a\nprivileged: true" }, "settings.message"},
		"nul in message":      {func(s *agentapi.Spec) { s.Settings["message"] = "a\x00b" }, "settings.message"},
		"undeclared secret":   {func(s *agentapi.Spec) { s.Secrets["other"] = "abc" }, "secrets.other"},
		"missing secret":      {func(s *agentapi.Spec) { delete(s.Secrets, "db_password") }, "secrets.db_password"},
		"secret with quote":   {func(s *agentapi.Spec) { s.Secrets["db_password"] = secretValue }, "secrets.db_password"},
		"bad hostname":        {func(s *agentapi.Spec) { s.Hosts["web"] = "evil host" }, "hosts.web"},
		"undeclared host":     {func(s *agentapi.Spec) { s.Hosts["admin"] = "a.example" }, "hosts.admin"},
		"missing host":        {func(s *agentapi.Spec) { delete(s.Hosts, "web") }, "hosts.web"},
		"url with user info":  {func(s *agentapi.Spec) { s.URLs.Issuer = "http://u:p@orglane.localhost" }, "urls.issuer"},
		"url not http":        {func(s *agentapi.Spec) { s.URLs.Webhook = "file:///etc/passwd" }, "urls.webhook"},
		"other package":       {func(s *agentapi.Spec) { s.Package.Version = "2.0.0-1" }, "package"},
		"bad state":           {func(s *agentapi.Spec) { s.State = "exec" }, "state"},
		"purge while running": {func(s *agentapi.Spec) { s.Purge = true }, "purge"},
		"no generation":       {func(s *agentapi.Spec) { s.Generation = 0 }, "generation"},
		"bad ingress":         {func(s *agentapi.Spec) { s.Ingress = "tunnel" }, "ingress_mode"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			spec := testSpec()
			c.mutate(spec)
			_, err := p.Inputs(spec)
			var ie *InputError
			if !errors.As(err, &ie) {
				t.Fatalf("error = %v, want InputError", err)
			}
			if !slices.Contains(ie.Fields, c.field) {
				t.Fatalf("fields = %v, want %s", ie.Fields, c.field)
			}
			if strings.Contains(err.Error(), "$ecret") || strings.Contains(err.Error(), "evil") {
				t.Fatalf("error %q contains an input value", err)
			}
		})
	}
}

func TestQuote(t *testing.T) {
	for _, s := range []string{
		"plain", `with "quotes"`, `back\slash`, "${ORGLANE_AGENT_SECRET}", "$(id)", "a: b", "- x", "{{.Secrets}}",
		"#comment", "tab\there", "uni code", "\x7f", "ünïcødé ✓", "'single'", "]}", "*anchor &a",
	} {
		var got string
		if err := yaml.Unmarshal([]byte("v: "+Quote(s)), &struct {
			V *string `yaml:"v"`
		}{&got}); err != nil {
			t.Fatalf("Quote(%q) = %s is not YAML: %v", s, Quote(s), err)
		}
		// Compose turns "$$" back into "$" without interpolating.
		if want := strings.ReplaceAll(s, "$", "$$"); got != want {
			t.Errorf("Quote(%q) decodes to %q, want %q", s, got, want)
		}
	}
}

func renderTest(t *testing.T, spec *agentapi.Spec, rc RenderContext) (*Rendered, Inputs) {
	t.Helper()
	p := testPackage(t)
	in, err := p.Inputs(spec)
	if err != nil {
		t.Fatal(err)
	}
	r, err := p.Render(spec, in, rc)
	if err != nil {
		t.Fatal(err)
	}
	return r, in
}

func TestRender(t *testing.T) {
	rc := testContext(t)
	r, _ := renderTest(t, testSpec(), rc)
	var project struct {
		Name     string `yaml:"name"`
		Services map[string]struct {
			Image       string            `yaml:"image"`
			Environment map[string]string `yaml:"environment"`
			Ports       []string          `yaml:"ports"`
			Networks    map[string]struct {
				Aliases []string `yaml:"aliases"`
			} `yaml:"networks"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(r.Compose, &project); err != nil {
		t.Fatalf("rendered compose is not YAML: %v\n%s", err, r.Compose)
	}
	if project.Name != "orglane-at1" {
		t.Errorf("name = %q", project.Name)
	}
	web := project.Services["web"]
	if !slices.Equal(web.Ports, []string{"127.0.0.1:18081:8080"}) {
		t.Errorf("ports = %v", web.Ports)
	}
	if !slices.Equal(web.Networks["edge"].Aliases, []string{"at1-web"}) {
		t.Errorf("aliases = %v", web.Networks["edge"].Aliases)
	}
	worker := project.Services["worker"]
	if worker.Environment["PGURL"] != "postgresql://svc_at1@postgres:5432/svc_at1" || worker.Environment["PGPASSWORD"] != "${ORGLANE_SECRET_DB_PASSWORD}" {
		t.Errorf("worker environment = %v", worker.Environment)
	}
	if string(r.Env) != "ORGLANE_SECRET_DB_PASSWORD='c2VjcmV0LXBhc3N3b3Jk'\n" {
		t.Errorf("env = %q", r.Env)
	}
	if strings.Contains(string(r.Compose), "c2VjcmV0LXBhc3N3b3Jk") {
		t.Error("the secret value is in the compose file")
	}
	if f, ok := r.Files["web.sh"]; !ok || f.Mode != 0o755 {
		t.Errorf("files = %v", slices.Collect(maps.Keys(r.Files)))
	}
	again, _ := renderTest(t, testSpec(), rc)
	if again.Hash() != r.Hash() {
		t.Error("rendering is not deterministic")
	}
	changed := testSpec()
	changed.Settings["message"] = "other"
	if c, _ := renderTest(t, changed, rc); c.Hash() == r.Hash() {
		t.Error("a changed setting does not change the hash")
	}

	rc.Fenced = true
	fenced, _ := renderTest(t, testSpec(), rc)
	if err := yaml.Unmarshal(fenced.Compose, &project); err != nil {
		t.Fatal(err)
	}
	if web := project.Services["web"]; len(web.Ports) != 0 || !slices.Equal(web.Networks["edge"].Aliases, []string{"at1-web-fenced"}) {
		t.Errorf("fenced render: ports %v, aliases %v", web.Ports, web.Networks["edge"].Aliases)
	}
}

// TestRenderInjection checks that hostile setting values stay one inert
// scalar: they add no keys and Compose does not interpolate them.
func TestRenderInjection(t *testing.T) {
	for _, msg := range []string{
		`x" privileged: true #`,
		`${ORGLANE_AGENT_SECRET}`,
		`$(id) and ${HOME:-/}`,
		`{{.Secrets}}`,
		`'] }, privileged: true, x: [{'`,
		"a: b, c: [d]",
	} {
		spec := testSpec()
		spec.Settings["message"] = msg
		r, _ := renderTest(t, spec, testContext(t))
		var project struct {
			Services map[string]map[string]any `yaml:"services"`
		}
		if err := yaml.Unmarshal(r.Compose, &project); err != nil {
			t.Fatalf("%q: %v", msg, err)
		}
		web := project.Services["web"]
		if _, ok := web["privileged"]; ok {
			t.Fatalf("%q injected a key", msg)
		}
		env, _ := web["environment"].(map[string]any)
		if want := strings.ReplaceAll(msg, "$", "$$"); env["MESSAGE"] != want {
			t.Fatalf("MESSAGE = %q, want %q", env["MESSAGE"], want)
		}
		if got := slices.Sorted(maps.Keys(project.Services)); !slices.Equal(got, []string{"web", "worker"}) {
			t.Fatalf("services = %v", got)
		}
	}
}

// normalized returns the project as `docker compose config --format json`
// prints it for the agenttest package rendered with rc (checked against the
// real Compose in the integration test).
func normalized(rc RenderContext) map[string]any {
	logging := map[string]any{"driver": "json-file", "options": map[string]any{"max-file": "2", "max-size": "1m"}}
	files := map[string]any{"type": "bind", "source": rc.FilesPath(), "target": "/orglane/files", "read_only": true, "bind": map[string]any{}}
	return map[string]any{
		"name": rc.Project(),
		"networks": map[string]any{
			"default":  map[string]any{"name": rc.Project() + "_default", "ipam": map[string]any{}},
			"edge":     map[string]any{"name": rc.EdgeNetwork, "ipam": map[string]any{}, "external": true},
			"services": map[string]any{"name": rc.ServicesNetwork, "ipam": map[string]any{}, "external": true},
		},
		"services": map[string]any{
			"web": map[string]any{
				"command":     []any{"sh", "/orglane/files/web.sh"},
				"entrypoint":  nil,
				"environment": map[string]any{"MESSAGE": "hi"},
				"healthcheck": map[string]any{"test": []any{"CMD", "wget"}, "interval": "2s"},
				"image":       "docker.io/library/busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec",
				"logging":     logging,
				"networks":    map[string]any{"default": nil, "edge": map[string]any{"aliases": []any{"at1-web"}}},
				"ports": []any{map[string]any{
					"mode": "ingress", "host_ip": "127.0.0.1", "target": float64(8080), "published": "18081", "protocol": "tcp",
				}},
				"restart":      "unless-stopped",
				"security_opt": []any{"no-new-privileges:true"},
				"volumes": []any{
					map[string]any{"type": "bind", "source": rc.DataPath("www"), "target": "/data", "bind": map[string]any{}},
					files,
				},
			},
			"worker": map[string]any{
				"command":     []any{"sleep", "2147483647"},
				"environment": map[string]any{"PGPASSWORD": "x"},
				"healthcheck": map[string]any{"test": []any{"CMD-SHELL", "psql"}},
				"image":       "docker.io/library/postgres:18.6-alpine3.24@sha256:10a5c69a83035ab5239f5620d708520e8b28f5aa1f2b20143e6024532edadfb5",
				"init":        true,
				"logging":     logging,
				"networks":    map[string]any{"default": nil, "services": nil},
				"restart":     "unless-stopped",
				"cap_drop":    []any{"ALL"},
				"volumes":     []any{files},
			},
		},
	}
}

func checkNormalized(t *testing.T, project map[string]any, rc RenderContext) error {
	t.Helper()
	p := testPackage(t)
	in, err := p.Inputs(testSpec())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	return p.CheckPolicy(raw, in, rc)
}

func svc(project map[string]any, name string) map[string]any {
	return project["services"].(map[string]any)[name].(map[string]any)
}

func TestPolicy(t *testing.T) {
	rc := testContext(t)
	if err := os.MkdirAll(rc.DataPath("www"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkNormalized(t, normalized(rc), rc); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(rc.DataPath("www"), "escape")); err != nil {
		t.Fatal(err)
	}
	bind := func(source string, ro bool) map[string]any {
		return map[string]any{"type": "bind", "source": source, "target": "/x", "read_only": ro}
	}
	addVolume := func(v map[string]any) func(map[string]any) {
		return func(p map[string]any) {
			w := svc(p, "web")
			w["volumes"] = append(w["volumes"].([]any), v)
		}
	}
	setWeb := func(k string, v any) func(map[string]any) {
		return func(p map[string]any) { svc(p, "web")[k] = v }
	}
	cases := map[string]struct {
		mutate func(map[string]any)
		rule   string
	}{
		"privileged":            {setWeb("privileged", true), RulePrivileged},
		"pid host":              {setWeb("pid", "host"), RuleHostNamespace},
		"ipc host":              {setWeb("ipc", "host"), RuleHostNamespace},
		"userns host":           {setWeb("userns_mode", "host"), RuleHostNamespace},
		"network_mode host":     {setWeb("network_mode", "host"), RuleNetworkMode},
		"network_mode service":  {setWeb("network_mode", "container:orglane-postgres-1"), RuleNetworkMode},
		"devices":               {setWeb("devices", []any{map[string]any{"source": "/dev/mem"}}), RuleDevices},
		"cgroup parent":         {setWeb("cgroup_parent", "/"), RuleDevices},
		"cap_add":               {setWeb("cap_add", []any{"SYS_ADMIN"}), RuleCapability},
		"cap_add all":           {setWeb("cap_add", []any{"ALL"}), RuleCapability},
		"seccomp unconfined":    {setWeb("security_opt", []any{"seccomp=unconfined"}), RuleSecurityOpt},
		"apparmor unconfined":   {setWeb("security_opt", []any{"apparmor=unconfined"}), RuleSecurityOpt},
		"unpinned image":        {setWeb("image", "docker.io/library/busybox:latest"), RuleImage},
		"build":                 {setWeb("build", map[string]any{"context": "/"}), RuleServiceKey},
		"volumes_from":          {setWeb("volumes_from", []any{"container:x"}), RuleServiceKey},
		"extra_hosts":           {setWeb("extra_hosts", []any{"orglane.localhost=10.0.0.1"}), RuleServiceKey},
		"container_name":        {setWeb("container_name", "postgres"), RuleServiceKey},
		"compose label":         {setWeb("labels", map[string]any{"com.docker.compose.project": "orglane"}), RuleLabel},
		"host bind mount":       {addVolume(bind("/etc", true)), RuleBindMount},
		"docker socket":         {addVolume(bind("/var/run/docker.sock", false)), RuleDockerSocket},
		"relative escape":       {addVolume(bind(rc.DataPath("www")+"/../../../..", false)), RuleBindMount},
		"symlink escape":        {addVolume(bind(filepath.Join(rc.DataPath("www"), "escape"), false)), RuleBindMount},
		"instance dir itself":   {addVolume(bind(rc.InstanceDir, false)), RuleBindMount},
		"writable files":        {addVolume(bind(rc.FilesPath(), false)), RuleFilesWritable},
		"named volume":          {addVolume(map[string]any{"type": "volume", "source": "data", "target": "/x"}), RuleNamedVolume},
		"bind propagation":      {addVolume(map[string]any{"type": "bind", "source": rc.DataPath("www"), "target": "/x", "bind": map[string]any{"propagation": "rshared"}}), RuleBindMount},
		"top-level volume":      {func(p map[string]any) { p["volumes"] = map[string]any{"data": map[string]any{}} }, RuleNamedVolume},
		"top-level configs":     {func(p map[string]any) { p["configs"] = map[string]any{"c": map[string]any{"file": "/etc/shadow"}} }, RuleProjectKey},
		"project name":          {func(p map[string]any) { p["name"] = "orglane" }, RuleProjectName},
		"undeclared port":       {setWeb("ports", []any{map[string]any{"host_ip": "127.0.0.1", "target": float64(2375), "published": "2375"}}), RulePort},
		"port on all addresses": {setWeb("ports", []any{map[string]any{"host_ip": "", "target": float64(8080), "published": "18081"}}), RulePort},
		"port other host port":  {setWeb("ports", []any{map[string]any{"host_ip": "127.0.0.1", "target": float64(8080), "published": "80"}}), RulePort},
		"alias hijack edge": {func(p map[string]any) {
			svc(p, "web")["networks"] = map[string]any{"edge": map[string]any{"aliases": []any{"orglane.localhost"}}}
		}, RuleAlias},
		"alias on services": {func(p map[string]any) {
			svc(p, "worker")["networks"] = map[string]any{"services": map[string]any{"aliases": []any{"postgres"}}}
		}, RuleAlias},
		"fixed address": {func(p map[string]any) {
			svc(p, "web")["networks"] = map[string]any{"edge": map[string]any{"ipv4_address": "10.231.0.2"}}
		}, RuleNetworkSettings},
		"foreign network": {func(p map[string]any) {
			p["networks"].(map[string]any)["db"] = map[string]any{"name": "orglane_database", "external": true}
		}, RuleNetwork},
		"host network driver": {func(p map[string]any) {
			p["networks"].(map[string]any)["default"].(map[string]any)["driver"] = "host"
		}, RuleNetwork},
		"undeclared service": {func(p map[string]any) {
			p["services"].(map[string]any)["extra"] = map[string]any{"image": svc(p, "web")["image"], "restart": "unless-stopped"}
		}, RuleService},
		"one-shot with port": {func(p map[string]any) {
			p["services"].(map[string]any)["extra"] = map[string]any{
				"image": svc(p, "web")["image"], "restart": "no",
				"ports": []any{map[string]any{"target": float64(1), "published": "1"}},
			}
		}, RuleOneShot},
		// The reviewed collision: an unfenced alias in the fenced namespace
		// would let a route reach another version's fenced project.
		"fence suffix alias": {func(p map[string]any) {
			p["services"].(map[string]any)["web-fenced"] = map[string]any{
				"image": svc(p, "web")["image"], "restart": "no",
				"networks": map[string]any{"edge": map[string]any{"aliases": []any{"at1-web-fenced"}}},
			}
		}, RuleAlias},
		"missing service":     {func(p map[string]any) { delete(p["services"].(map[string]any), "worker") }, RuleService},
		"restart always":      {setWeb("restart", "always"), RuleRestart},
		"no healthcheck":      {setWeb("healthcheck", nil), RuleHealthcheck},
		"healthcheck off":     {setWeb("healthcheck", map[string]any{"disable": true}), RuleHealthcheck},
		"no log rotation":     {setWeb("logging", map[string]any{"driver": "json-file"}), RuleLogRotation},
		"syslog logging":      {setWeb("logging", map[string]any{"driver": "syslog"}), RuleLogRotation},
		"unknown service key": {setWeb("runtime", "runc"), RuleServiceKey},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := normalized(rc)
			c.mutate(p)
			err := checkNormalized(t, p, rc)
			var pe *PolicyError
			if !errors.As(err, &pe) {
				t.Fatalf("error = %v, want a policy violation", err)
			}
			if pe.Rule != c.rule {
				t.Fatalf("rule = %s (%v), want %s", pe.Rule, pe, c.rule)
			}
		})
	}

	// A one-shot service without ports is allowed.
	p := normalized(rc)
	p["services"].(map[string]any)["migrate"] = map[string]any{"image": svc(p, "worker")["image"], "restart": "no"}
	if err := checkNormalized(t, p, rc); err != nil {
		t.Fatalf("one-shot service: %v", err)
	}

	// A fenced project publishes no ports and uses only fenced aliases.
	fenced := rc
	fenced.Fenced = true
	if err := checkNormalized(t, normalized(rc), fenced); err == nil {
		t.Fatal("an unfenced project passed the fenced policy")
	}
	p = normalized(rc)
	delete(svc(p, "web"), "ports")
	svc(p, "web")["networks"] = map[string]any{"edge": map[string]any{"aliases": []any{"at1-web-fenced"}}}
	if err := checkNormalized(t, p, fenced); err != nil {
		t.Fatalf("fenced project: %v", err)
	}
}

func TestPolicyVariantsExist(t *testing.T) {
	for _, name := range agenttest.PolicyVariants() {
		src, rule, err := agenttest.PolicyVariant(name)
		if err != nil {
			t.Fatal(err)
		}
		if rule == "" {
			t.Fatalf("%s: no rule", name)
		}
		r, err := NewRegistry(src)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		p, _ := r.Get(agentapi.PackageRef{ID: "agenttest", Version: agenttest.Version})
		spec := testSpec()
		in, err := p.Inputs(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Render(spec, in, testContext(t)); err != nil {
			t.Fatalf("%s: render: %v", name, err)
		}
	}
}
