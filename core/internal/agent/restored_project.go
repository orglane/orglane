package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/agentapi"
	"github.com/orglane/orglane/core/internal/packages"
)

const restoredProjectFile = "restored-project.json"

// restoredProject keeps only deployment files, never snapshot data. It remains
// usable after snapshot retention removes the source or the process restarts.
// The exact spec binds it to the version and inputs successfully exposed.
type restoredProject struct {
	Spec       *agentapi.Spec     `json:"spec"`
	Rendered   *packages.Rendered `json:"rendered"`
	Configured []byte             `json:"configured,omitempty"`
}

func (a *Agent) saveRestoredProject(key string, id uuid.UUID, spec *agentapi.Spec) error {
	r, configured, err := a.snapshotRenderedProject(key, id)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(a.instanceDir(key), restoredProjectFile), restoredProject{Spec: spec, Rendered: r, Configured: configured})
}

// repairRestored converges a rolled-back version absent from this binary.
// It never runs restore hooks or replaces data, including after exposure and
// subsequent client writes. New desired inputs require an embedded package.
func (a *Agent) repairRestored(ctx context.Context, key string, spec *agentapi.Spec) error {
	data, err := os.ReadFile(filepath.Join(a.instanceDir(key), restoredProjectFile)) //nolint:gosec // agent-owned path
	if err != nil {
		return failed(agentapi.CodeUnknownPackageVersion, err)
	}
	var saved restoredProject
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&saved); err != nil {
		return failed(agentapi.CodeInternal, err)
	}
	if saved.Spec == nil || saved.Rendered == nil || saved.Spec.Hash() != spec.Hash() {
		return failed(agentapi.CodeUnknownPackageVersion, fmt.Errorf("no saved project for package %s generation %d", spec.Package, spec.Generation))
	}
	if err := writeRendered(a.renderContext(key), saved.Rendered); err != nil {
		return failed(agentapi.CodeComposeFailed, err)
	}
	p := filepath.Join(a.instanceDir(key), configuredFile)
	if saved.Configured != nil {
		if err := writeFileAtomic(p, saved.Configured, 0o600); err != nil {
			return failed(agentapi.CodeInternal, err)
		}
	} else if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(agentapi.CodeInternal, err)
	}
	c := a.compose(key, spec.SecretValues())
	switch spec.State {
	case agentapi.StateRunning:
		return a.upWait(ctx, c, defaultStartTimeout)
	case agentapi.StateStopped:
		return c.stop(ctx)
	default:
		return c.down(ctx)
	}
}
