package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/orglane/orglane/core/internal/packages"
)

// fenceCompose returns a rendered compose file with the write fence applied
// (docs/specs/agent.md "Upgrade"): no service publishes a host port, and
// every network alias gets the fenced suffix, so neither the routes'
// upstream aliases nor published ports reach the project.
//
// The upgrade candidate is rendered fenced by its package (the template also
// sees Fenced). A snapshot's rendered files are of an earlier version, whose
// package this binary may no longer embed, so the rollback fences them with
// this transformation; checkFenced verifies the normalized result.
func fenceCompose(data []byte) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("fence: the compose file is not a mapping")
	}
	services := mappingValue(doc.Content[0], "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return nil, errors.New("fence: the compose file has no services")
	}
	for i := 1; i < len(services.Content); i += 2 {
		svc := services.Content[i]
		if svc.Kind != yaml.MappingNode {
			return nil, errors.New("fence: malformed service")
		}
		deleteKey(svc, "ports")
		nets := mappingValue(svc, "networks")
		if nets == nil || nets.Kind != yaml.MappingNode {
			continue // a list of networks has no aliases
		}
		for j := 1; j < len(nets.Content); j += 2 {
			n := nets.Content[j]
			if n.Kind != yaml.MappingNode {
				continue
			}
			aliases := mappingValue(n, "aliases")
			if aliases == nil {
				continue
			}
			if aliases.Kind != yaml.SequenceNode {
				return nil, errors.New("fence: malformed aliases")
			}
			for _, a := range aliases.Content {
				if a.Kind != yaml.ScalarNode {
					return nil, errors.New("fence: malformed alias")
				}
				if !strings.HasSuffix(a.Value, packages.FencedSuffix) {
					a.Value += packages.FencedSuffix
				}
			}
		}
	}
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("fence: %w", err)
	}
	return buf.Bytes(), nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func deleteKey(m *yaml.Node, key string) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return
		}
	}
}

// checkFenced verifies a project normalized by `docker compose config
// --format json`: no published ports, and only fenced aliases.
func checkFenced(normalized []byte) error {
	var p struct {
		Services map[string]struct {
			Ports    []any `json:"ports"`
			Networks map[string]*struct {
				Aliases []string `json:"aliases"`
			} `json:"networks"`
		} `json:"services"`
	}
	if err := json.Unmarshal(normalized, &p); err != nil {
		return fmt.Errorf("fence: %w", err)
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
				if !strings.HasSuffix(a, packages.FencedSuffix) {
					return fmt.Errorf("fence: service %s has the alias %s on network %s", name, a, net)
				}
			}
		}
	}
	return nil
}
