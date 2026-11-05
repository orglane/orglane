package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/packages"
)

// The write fence (docs/specs/agent.md "Upgrade") keeps every route of the
// instance away from a fenced project: the upgrade candidate and a restored
// version until it is exposed. A route reaches a container by a name in
// Docker's DNS, so the fence is about names, not about how a compose file
// spells them:
//
//   - routed names are the names a route of the instance may target while
//     one of the involved versions is routed: <key>-<service> for every
//     service of an unfenced project, and every explicit alias in it
//     (routedNames);
//   - Docker registers for each container, on each of its networks, its
//     explicit aliases, the service name (Compose), the container name
//     (container_name, or <project>-<service>-<n>), the hostname and the
//     short container ID. The ID is hexadecimal and routed names always
//     contain "-", so it never collides; every other name of a fenced
//     project must not be a routed name (checkFenced).
//
// Both sides are read from projects normalized by `docker compose config
// --format json`, so YAML anchors, merge keys and extends are resolved.

// dnsProject is the part of a normalized project that decides which names
// Docker's DNS gives its containers.
type dnsProject struct {
	Services map[string]struct {
		Ports         []any  `json:"ports"`
		Hostname      string `json:"hostname"`
		ContainerName string `json:"container_name"`
		Networks      map[string]*struct {
			Aliases []string `json:"aliases"`
		} `json:"networks"`
	} `json:"services"`
}

func parseDNSProject(normalized []byte) (*dnsProject, error) {
	var p dnsProject
	if err := json.Unmarshal(normalized, &p); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	return &p, nil
}

// routedNames returns the names a route of instance key may target while
// the normalized, unfenced project is routed.
func routedNames(key string, normalized []byte) ([]string, error) {
	p, err := parseDNSProject(normalized)
	if err != nil {
		return nil, err
	}
	var out []string
	for name, s := range p.Services {
		out = append(out, packages.ServiceAlias(key, name, false))
		for _, n := range s.Networks {
			if n != nil {
				out = append(out, n.Aliases...)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// mergeNames returns the sorted union of name lists.
func mergeNames(lists ...[]string) []string {
	var out []string
	for _, l := range lists {
		out = append(out, l...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// checkFenced verifies a normalized project of the instance's Compose
// project: no published ports, only fenced explicit aliases, and no name
// Docker registers for its containers is a routed name.
func checkFenced(normalized []byte, project string, routed []string) error {
	p, err := parseDNSProject(normalized)
	if err != nil {
		return err
	}
	for name, s := range p.Services {
		if len(s.Ports) > 0 {
			return fmt.Errorf("fence: service %s publishes ports", name)
		}
		for net, n := range s.Networks {
			if n == nil {
				continue
			}
			for _, a := range n.Aliases {
				if slices.Contains(routed, a) || !strings.HasSuffix(a, packages.FencedSuffix) {
					return fmt.Errorf("fence: service %s has the alias %s on network %s", name, a, net)
				}
			}
		}
	}
	return checkImplicitNames(p, project, routed)
}

// checkImplicitNames verifies that no name Docker registers for a
// project's containers without an explicit alias (service name, hostname,
// container name) is a routed name. The running version is checked with it
// before an upgrade, because a rollback restores it fenced.
func checkImplicitNames(p *dnsProject, project string, routed []string) error {
	for name, s := range p.Services {
		for _, n := range []string{name, s.Hostname, s.ContainerName} {
			if n != "" && slices.Contains(routed, n) {
				return fmt.Errorf("fence: service %s answers to the routed name %s", name, n)
			}
		}
		if s.ContainerName == "" {
			// Compose names containers <project>-<service>-<n>.
			prefix := project + "-" + name + "-"
			for _, r := range routed {
				if strings.HasPrefix(r, prefix) {
					return fmt.Errorf("fence: a container of service %s may answer to the routed name %s", name, r)
				}
			}
		}
	}
	return nil
}

// fenceCompose returns a rendered compose file with the write fence
// applied: no service publishes a host port, and every explicit network
// alias gets the fenced suffix, repeated until it is no routed name. YAML
// anchors, aliases and merge keys are resolved first, so an alias spelled
// through them is fenced too.
//
// The upgrade candidate is rendered fenced by its package (the template also
// sees Fenced). A snapshot's rendered files are of an earlier version, whose
// package this binary may no longer embed, so the rollback fences them with
// this transformation; checkFenced verifies the normalized result, which is
// what counts (Compose features this transformation does not rewrite, such
// as extends from another file, fail the check and the rollback stays
// fenced).
func fenceCompose(data []byte, routed []string) ([]byte, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	if doc == nil {
		return nil, errors.New("fence: the compose file is not a mapping")
	}
	services, ok := doc["services"].(map[string]any)
	if !ok || len(services) == 0 {
		return nil, errors.New("fence: the compose file has no services")
	}
	for name, raw := range services {
		svc, ok := raw.(map[string]any)
		if !ok {
			return nil, errors.New("fence: malformed service")
		}
		// Decoded YAML aliases may share values: build new ones instead of
		// changing them in place.
		out := make(map[string]any, len(svc))
		for k, v := range svc {
			if k != "ports" {
				out[k] = v
			}
		}
		if nets, ok := svc["networks"].(map[string]any); ok {
			fencedNets := make(map[string]any, len(nets))
			for net, nv := range nets {
				n, ok := nv.(map[string]any)
				if !ok || n["aliases"] == nil {
					fencedNets[net] = nv
					continue
				}
				list, ok := n["aliases"].([]any)
				if !ok {
					return nil, errors.New("fence: malformed aliases")
				}
				fencedList := make([]any, 0, len(list))
				for _, a := range list {
					s, ok := a.(string)
					if !ok {
						return nil, errors.New("fence: malformed alias")
					}
					for {
						s += packages.FencedSuffix
						if !slices.Contains(routed, s) {
							break
						}
					}
					fencedList = append(fencedList, s)
				}
				fn := make(map[string]any, len(n))
				for k, v := range n {
					fn[k] = v
				}
				fn["aliases"] = fencedList
				fencedNets[net] = fn
			}
			out["networks"] = fencedNets
		}
		services[name] = out
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	return buf.Bytes(), nil
}
