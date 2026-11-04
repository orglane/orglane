package packages

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Policy rules (docs/specs/agent.md "Policy check"). A violation names its
// rule; nothing of the project is started.
const (
	RuleProjectKey      = "project_key_not_allowed"
	RuleProjectName     = "project_name"
	RuleNamedVolume     = "named_volume"
	RuleNetwork         = "network_not_allowed"
	RuleService         = "service_not_declared"
	RuleOneShot         = "one_shot_service"
	RuleServiceKey      = "service_key_not_allowed"
	RuleImage           = "image_not_pinned"
	RulePrivileged      = "privileged"
	RuleHostNamespace   = "host_namespace"
	RuleNetworkMode     = "network_mode"
	RuleDevices         = "devices"
	RuleCapability      = "capability_not_allowed"
	RuleSecurityOpt     = "security_opt"
	RuleBindMount       = "bind_mount_outside_instance"
	RuleFilesWritable   = "files_mount_writable"
	RuleDockerSocket    = "docker_socket"
	RuleMountType       = "mount_type_not_allowed"
	RulePort            = "port_not_declared"
	RuleAlias           = "alias_not_allowed"
	RuleNetworkSettings = "network_settings_not_allowed"
	RuleRestart         = "restart_policy"
	RuleLogRotation     = "log_rotation"
	RuleHealthcheck     = "healthcheck_missing"
	RuleLabel           = "label_not_allowed"
)

// PolicyError is a policy violation.
type PolicyError struct {
	Rule    string
	Service string
	Detail  string
}

func (e *PolicyError) Error() string {
	if e.Service != "" {
		return fmt.Sprintf("policy violation %s in service %s: %s", e.Rule, e.Service, e.Detail)
	}
	return fmt.Sprintf("policy violation %s: %s", e.Rule, e.Detail)
}

// allowedServiceKeys are the service keys a managed project may use. Every
// other key of the Compose specification (build, configs, secrets,
// volumes_from, extra_hosts, container_name, sysctls, runtime, ...) is
// refused, so new Compose features are off until reviewed. Keys that grant
// host access have their own rules and are checked first.
var allowedServiceKeys = []string{
	"cap_add", "cap_drop", "command", "depends_on", "entrypoint", "environment",
	"expose", "healthcheck", "image", "init", "labels", "logging", "networks",
	"ports", "pull_policy", "read_only", "restart", "security_opt", "shm_size",
	"stop_grace_period", "stop_signal", "tmpfs", "user", "volumes", "working_dir",
}

// noNewPrivileges are the accepted security_opt values: the policy allows
// tightening only.
var noNewPrivileges = []string{"no-new-privileges", "no-new-privileges:true", "no-new-privileges=true"}

// dockerSockets are paths of the Docker socket on common hosts.
var dockerSockets = []string{"/var/run/docker.sock", "/run/docker.sock"}

// CheckPolicy checks a project normalized by `docker compose config
// --format json` against the package policy. rc must be the context the
// project was rendered with, and in the inputs it was rendered from.
func (p *Package) CheckPolicy(projectJSON []byte, in Inputs, rc RenderContext) error {
	var project map[string]any
	if err := json.Unmarshal(projectJSON, &project); err != nil {
		return fmt.Errorf("policy: decode project: %w", err)
	}
	c := &policyCheck{p: p, m: p.Manifest, in: in, rc: rc}
	return c.project(project)
}

type policyCheck struct {
	p  *Package
	m  *Manifest
	in Inputs
	rc RenderContext
	// networks maps the project's network keys to their kind: "default",
	// "edge" or "services".
	networks map[string]string
	// instanceDir is rc.InstanceDir with symlinks resolved.
	instanceDir string
}

func violation(rule, service, format string, args ...any) error {
	return &PolicyError{Rule: rule, Service: service, Detail: fmt.Sprintf(format, args...)}
}

func (c *policyCheck) project(project map[string]any) error {
	for k, v := range project {
		switch {
		case v == nil || strings.HasPrefix(k, "x-"):
		case k == "name" || k == "services" || k == "networks":
		case k == "volumes":
			if m, _ := v.(map[string]any); len(m) > 0 {
				return violation(RuleNamedVolume, "", "named volumes are not allowed")
			}
		default:
			return violation(RuleProjectKey, "", "top-level key %q is not allowed", k)
		}
	}
	if project["name"] != c.rc.Project() {
		return violation(RuleProjectName, "", "project name %v, want %s", project["name"], c.rc.Project())
	}
	dir, err := resolvePath(c.rc.InstanceDir)
	if err != nil {
		return fmt.Errorf("policy: resolve instance directory: %w", err)
	}
	c.instanceDir = dir
	if err := c.topNetworks(project["networks"]); err != nil {
		return err
	}
	services, ok := project["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return violation(RuleService, "", "the project has no services")
	}
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		svc, ok := services[name].(map[string]any)
		if !ok {
			return violation(RuleService, name, "malformed service")
		}
		if err := c.service(name, svc); err != nil {
			return err
		}
	}
	for _, name := range c.m.Services {
		if _, ok := services[name]; !ok {
			return violation(RuleService, name, "declared service is missing from the project")
		}
	}
	return nil
}

// topNetworks allows the project's own default network and the external
// edge and services networks.
func (c *policyCheck) topNetworks(v any) error {
	c.networks = map[string]string{}
	nets, _ := v.(map[string]any)
	for key, raw := range nets {
		n, ok := raw.(map[string]any)
		if !ok {
			return violation(RuleNetwork, "", "network %q is malformed", key)
		}
		name, _ := n["name"].(string)
		external, _ := n["external"].(bool)
		for k, val := range n {
			switch k {
			case "name", "external":
			case "ipam":
				if m, _ := val.(map[string]any); len(m) > 0 {
					return violation(RuleNetwork, "", "network %q: ipam is not allowed", key)
				}
			case "internal":
				if external {
					return violation(RuleNetwork, "", "network %q: internal is not allowed on external networks", key)
				}
			default:
				if val != nil {
					return violation(RuleNetwork, "", "network %q: %s is not allowed", key, k)
				}
			}
		}
		switch {
		case external && name == c.rc.EdgeNetwork:
			c.networks[key] = "edge"
		case external && name == c.rc.ServicesNetwork:
			c.networks[key] = "services"
		case !external && key == "default" && name == c.rc.Project()+"_default":
			c.networks[key] = "default"
		default:
			return violation(RuleNetwork, "", "network %q (%s) is not allowed", key, name)
		}
	}
	return nil
}

func (c *policyCheck) service(name string, svc map[string]any) error {
	declared := c.m.HasService(name)
	restart, _ := svc["restart"].(string)
	if !declared {
		// One-shot services: restart "no" and no published ports.
		if restart != "no" {
			return violation(RuleService, name, "only declared services may run; one-shot services need restart \"no\"")
		}
		if ports, _ := svc["ports"].([]any); len(ports) > 0 {
			return violation(RuleOneShot, name, "one-shot services may not publish ports")
		}
	}

	// Rules with their own names first, then the key allowlist.
	if v, _ := svc["privileged"].(bool); v {
		return violation(RulePrivileged, name, "privileged is not allowed")
	}
	for _, k := range []string{"pid", "ipc", "uts", "userns_mode", "cgroup"} {
		if v, ok := svc[k].(string); ok && v != "" && v != "private" {
			return violation(RuleHostNamespace, name, "%s: %s is not allowed", k, v)
		}
	}
	if v, ok := svc["network_mode"].(string); ok && v != "" {
		return violation(RuleNetworkMode, name, "network_mode %s is not allowed; use networks", v)
	}
	for _, k := range []string{"devices", "device_cgroup_rules", "cgroup_parent", "gpus"} {
		if present(svc[k]) {
			return violation(RuleDevices, name, "%s is not allowed", k)
		}
	}
	for k, v := range svc {
		if v == nil || strings.HasPrefix(k, "x-") {
			continue
		}
		if !slices.Contains(allowedServiceKeys, k) {
			return violation(RuleServiceKey, name, "key %q is not allowed", k)
		}
	}

	image, _ := svc["image"].(string)
	if !slices.Contains(imageRefs(c.m), image) {
		return violation(RuleImage, name, "image %q is not a pinned image of the manifest", image)
	}
	if err := c.capabilities(name, svc["cap_add"]); err != nil {
		return err
	}
	for _, o := range strings2(svc["security_opt"]) {
		if !slices.Contains(noNewPrivileges, o) {
			return violation(RuleSecurityOpt, name, "security_opt %q is not allowed", o)
		}
	}
	for k := range anyMap(svc["labels"]) {
		if strings.HasPrefix(k, "com.docker.") {
			return violation(RuleLabel, name, "label %q is reserved for Docker", k)
		}
	}
	if err := c.volumes(name, svc["volumes"]); err != nil {
		return err
	}
	if err := c.ports(name, svc["ports"]); err != nil {
		return err
	}
	if err := c.serviceNetworks(name, svc["networks"]); err != nil {
		return err
	}
	if declared {
		if restart != "unless-stopped" {
			return violation(RuleRestart, name, "restart must be unless-stopped")
		}
		hc := anyMap(svc["healthcheck"])
		if disabled, _ := hc["disable"].(bool); disabled || !present(hc["test"]) || isNoneTest(hc["test"]) {
			return violation(RuleHealthcheck, name, "declared services need a healthcheck")
		}
		if !rotatesLogs(anyMap(svc["logging"])) {
			return violation(RuleLogRotation, name, "logging must rotate (json-file with max-size, or local)")
		}
	}
	return nil
}

func (c *policyCheck) capabilities(service string, v any) error {
	for _, raw := range strings2(v) {
		capName := strings.TrimPrefix(strings.ToUpper(raw), "CAP_")
		if capName == "ALL" || !slices.Contains(c.m.CapabilitiesAdd, capName) {
			return violation(RuleCapability, service, "capability %s is not in capabilities_add", raw)
		}
	}
	return nil
}

func (c *policyCheck) volumes(service string, v any) error {
	vols, _ := v.([]any)
	dataRoot := filepath.Join(c.instanceDir, "data")
	filesRoot := filepath.Join(c.instanceDir, FilesDir)
	for _, raw := range vols {
		vol := anyMap(raw)
		typ, _ := vol["type"].(string)
		source, _ := vol["source"].(string)
		target, _ := vol["target"].(string)
		switch typ {
		case "tmpfs":
			continue
		case "bind":
		case "volume":
			return violation(RuleNamedVolume, service, "volume %q is not allowed", source)
		default:
			return violation(RuleMountType, service, "mount type %q is not allowed", typ)
		}
		if slices.Contains(dockerSockets, filepath.Clean(source)) || slices.Contains(dockerSockets, filepath.Clean(target)) || filepath.Base(source) == "docker.sock" {
			return violation(RuleDockerSocket, service, "the Docker socket may not be mounted")
		}
		if !filepath.IsAbs(source) {
			return violation(RuleBindMount, service, "bind source %q is not absolute", source)
		}
		resolved, err := resolvePath(filepath.Clean(source))
		if err != nil {
			return fmt.Errorf("policy: resolve %s: %w", source, err)
		}
		if slices.Contains(dockerSockets, resolved) {
			return violation(RuleDockerSocket, service, "the Docker socket may not be mounted")
		}
		// The source as written (with the instance directory resolved) and
		// the source with every symlink resolved must both stay inside.
		written := filepath.Clean(source)
		if root := filepath.Clean(c.rc.InstanceDir); within(root, written) {
			rel, err := filepath.Rel(root, written)
			if err != nil {
				return fmt.Errorf("policy: %w", err)
			}
			written = filepath.Join(c.instanceDir, rel)
		}
		readOnly, _ := vol["read_only"].(bool)
		switch {
		case within(dataRoot, written) && within(dataRoot, resolved):
		case within(filesRoot, written) && within(filesRoot, resolved):
			if !readOnly {
				return violation(RuleFilesWritable, service, "package files must be mounted read-only")
			}
		default:
			return violation(RuleBindMount, service, "bind source %q is outside the instance's data and files", source)
		}
		if b := anyMap(vol["bind"]); b != nil {
			if prop, _ := b["propagation"].(string); prop != "" && prop != "private" && prop != "rprivate" {
				return violation(RuleBindMount, service, "bind propagation %q is not allowed", prop)
			}
		}
	}
	return nil
}

func (c *policyCheck) ports(service string, v any) error {
	ports, _ := v.([]any)
	if len(ports) > 0 && c.rc.Fenced {
		return violation(RulePort, service, "a fenced project publishes no ports")
	}
	for _, raw := range ports {
		pt := anyMap(raw)
		target := intValue(pt["target"])
		published, _ := pt["published"].(string)
		hostIP, _ := pt["host_ip"].(string)
		proto, _ := pt["protocol"].(string)
		ok := slices.ContainsFunc(c.m.Ports, func(d Port) bool {
			want, _ := c.in.Settings[d.Setting].(int64)
			return d.Service == service && d.Container == target &&
				published == strconv.FormatInt(want, 10) && hostIP == c.rc.PublishAddress &&
				(proto == "" || proto == "tcp" || proto == "udp")
		})
		if !ok {
			return violation(RulePort, service, "port %s:%s:%d is not declared", hostIP, published, target)
		}
	}
	return nil
}

func (c *policyCheck) serviceNetworks(service string, v any) error {
	if v == nil {
		return nil
	}
	nets, ok := v.(map[string]any)
	if !ok {
		return violation(RuleNetwork, service, "malformed networks")
	}
	for key, raw := range nets {
		kind, ok := c.networks[key]
		if !ok {
			return violation(RuleNetwork, service, "network %q is not allowed", key)
		}
		for k, val := range anyMap(raw) {
			if k != "aliases" && val != nil {
				return violation(RuleNetworkSettings, service, "network %q: %s is not allowed", key, k)
			}
		}
		aliases := strings2(anyMap(raw)["aliases"])
		if len(aliases) == 0 {
			continue
		}
		want := ServiceAlias(c.rc.Key, service, c.rc.Fenced)
		if kind != "edge" || len(aliases) != 1 || aliases[0] != want {
			return violation(RuleAlias, service, "aliases %v on network %q are not allowed (only %s on the edge network)", aliases, key, want)
		}
	}
	return nil
}

func imageRefs(m *Manifest) []string {
	out := make([]string, 0, len(m.Images))
	for _, ref := range m.Images {
		out = append(out, ref)
	}
	return out
}

func rotatesLogs(l map[string]any) bool {
	driver, _ := l["driver"].(string)
	switch driver {
	case "local":
		return true
	case "json-file":
		size, _ := anyMap(l["options"])["max-size"].(string)
		return size != ""
	}
	return false
}

func isNoneTest(v any) bool {
	t := strings2(v)
	return len(t) > 0 && t[0] == "NONE"
}

func present(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case bool:
		return x
	}
	return true
}

func anyMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func strings2(v any) []string {
	switch x := v.(type) {
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, _ := e.(string)
			out = append(out, s)
		}
		return out
	case string:
		return []string{x}
	}
	return nil
}

func intValue(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(x)
		return n
	}
	return 0
}

func within(root, p string) bool {
	return p == root || strings.HasPrefix(p, root+string(filepath.Separator))
}

// resolvePath resolves symlinks in p. A path that does not exist yet (Docker
// would create it) is resolved through its nearest existing ancestor.
func resolvePath(p string) (string, error) {
	var rest []string
	cur := filepath.Clean(p)
	for {
		r, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(append([]string{r}, rest...)...), nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = append([]string{filepath.Base(cur)}, rest...)
		cur = parent
	}
}
