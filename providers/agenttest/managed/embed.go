// Package managed embeds the agenttest managed package: a tiny, pinned test
// package for the agent's tests (docs/specs/agent.md "Testing"). It is not a
// product. The registry compiles it in only with the build tag "agenttest"
// (core/internal/packages), so a production agent cannot install it.
//
// The package runs two services: "web", a BusyBox httpd that serves the
// "message" setting from a data directory on a published port and the edge
// network, and "worker", which holds a PostgreSQL client for the per-service
// database; its configure hook writes the message into that database.
//
// Three versions exercise upgrades (docs/specs/agent.md "Upgrade"):
// 2.0.0-1 upgrades from 1.0.0-1 and 3.0.0-1 only from 2.0.0-1 (a required
// stop). Both run a migration hook and take a "fault" setting that makes the
// healthcheck or the migration fail. The web service accepts writes on its
// published port (GET /cgi-bin/write?<text>, read back at /writes.log) and
// reports the running version at /version.
package managed

import (
	"embed"
	"fmt"
	"io/fs"
	"strings"
	"testing/fstest"
)

// FS holds the package versions, one directory per version.
//
//go:embed all:1.0.0-1 all:2.0.0-1 all:3.0.0-1
var FS embed.FS

// The package versions in FS. Version is the one the policy variants
// change.
const (
	Version  = "1.0.0-1"
	Version2 = "2.0.0-1"
	Version3 = "3.0.0-1"
)

// policyVariants are changes to the Compose template that the agent's
// policy must refuse, by the rule that refuses them. Each replaces a
// fragment of the template.
var policyVariants = map[string]struct{ Old, New, Rule string }{
	"privileged":       {"    # policy-test: web\n", "    privileged: true\n", "privileged"},
	"host_bind_mount":  {"    # policy-test: web-volumes\n", "      - /etc:/host-etc:ro\n", "bind_mount_outside_instance"},
	"docker_socket":    {"    # policy-test: web-volumes\n", "      - /var/run/docker.sock:/var/run/docker.sock\n", "docker_socket"},
	"unpinned_image":   {`    image: {{image "web" | quote}}`, `    image: "docker.io/library/busybox:latest"`, "image_not_pinned"},
	"undeclared_port":  {"    # policy-test: worker\n", "    ports: [\"127.0.0.1:15432:5432\"]\n", "port_not_declared"},
	"host_network":     {"    networks: {default: {}, services: {}}\n", "    network_mode: host\n", "network_mode"},
	"extra_capability": {"    # policy-test: web\n", "    cap_add: [SYS_ADMIN]\n", "capability_not_allowed"},
	"host_pid":         {"    # policy-test: web\n", "    pid: host\n", "host_namespace"},
	"device":           {"    # policy-test: web\n", "    devices: [\"/dev/kmsg:/dev/kmsg\"]\n", "devices"},
	"alias_hijack":     {"    networks: {default: {}, services: {}}\n", "    networks: {default: {}, services: {aliases: [postgres]}}\n", "alias_not_allowed"},
	"seccomp_off":      {"    security_opt: [\"no-new-privileges:true\"]\n    # policy-test: web\n", "    security_opt: [\"seccomp=unconfined\"]\n", "security_opt"},
	"undeclared_service": {"  # policy-test: services\n",
		"  extra:\n    image: {{image \"web\" | quote}}\n    restart: unless-stopped\n    command: [\"sleep\", \"3600\"]\n", "service_not_declared"},
}

// PolicyVariants returns the names of the policy test variants.
func PolicyVariants() []string {
	out := make([]string, 0, len(policyVariants))
	for name := range policyVariants {
		out = append(out, name)
	}
	return out
}

// PolicyVariant returns the package with the variant's Compose template (in
// the form NewRegistry loads) and the policy rule that must refuse it.
func PolicyVariant(name string) (fs.FS, string, error) {
	v, ok := policyVariants[name]
	if !ok {
		return nil, "", fmt.Errorf("agenttest: unknown policy variant %q", name)
	}
	out := fstest.MapFS{}
	err := fs.WalkDir(FS, Version, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(FS, p)
		if err != nil {
			return err
		}
		if p == Version+"/compose.yaml.tmpl" {
			if !strings.Contains(string(data), v.Old) {
				return fmt.Errorf("agenttest: variant %s: fragment not found", name)
			}
			data = []byte(strings.Replace(string(data), v.Old, v.New, 1))
		}
		out[p] = &fstest.MapFile{Data: data}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	return out, v.Rule, nil
}
