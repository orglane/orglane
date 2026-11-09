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

// readRestoredProject loads the project saved after a rollback or restore.
func (a *Agent) readRestoredProject(key string) (*restoredProject, error) {
	data, err := os.ReadFile(filepath.Join(a.instanceDir(key), restoredProjectFile)) //nolint:gosec // agent-owned path
	if err != nil {
		return nil, err
	}
	var saved restoredProject
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&saved); err != nil {
		return nil, err
	}
	return &saved, nil
}

// sameSpecExceptGeneration reports whether two specs differ only in their
// generation. The saved project and the running project are re-used for the
// next generation without accepting changed inputs.
func sameSpecExceptGeneration(a, b *agentapi.Spec) bool {
	if a == nil || b == nil {
		return false
	}
	ac, bc := *a, *b
	ac.Generation, bc.Generation = 0, 0
	return ac.Hash() == bc.Hash()
}

// startProject starts, stops or takes down the project as spec says, using
// the deployment files that are already rendered.
func (a *Agent) startProject(ctx context.Context, key string, spec *agentapi.Spec) error {
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

// repairRestored converges a rolled-back version absent from this binary.
// It never runs restore hooks or replaces data, including after exposure and
// subsequent client writes. New desired inputs require an embedded package.
//
// The saved project is preferred (it repairs missing deployment files after
// a rollback or restore). Without one, the running project is kept when the
// spec is the same apart from its generation: an upgrade that failed before
// the switch leaves the running deployment untouched, so no re-render is
// needed to acknowledge its blocked generation.
func (a *Agent) repairRestored(ctx context.Context, key string, spec *agentapi.Spec) error {
	saved, err := a.readRestoredProject(key)
	if err == nil && saved.Spec != nil && saved.Rendered != nil && sameSpecExceptGeneration(saved.Spec, spec) {
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
		return a.startProject(ctx, key, spec)
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return failed(agentapi.CodeInternal, err)
	}
	st, serr := loadState(a.instanceDir(key))
	if serr == nil && sameSpecExceptGeneration(st.current, spec) {
		return a.startProject(ctx, key, spec)
	}
	return failed(agentapi.CodeUnknownPackageVersion, fmt.Errorf("no saved project for package %s generation %d", spec.Package, spec.Generation))
}

// resolveRevert replaces the minimal revert spec with the full spec of the
// project the agent can apply for it: the saved restored project when it
// names the same instance and package, else the running spec. A revert that
// matches neither stays minimal and fails as unknown_package_version.
func (a *Agent) resolveRevert(key string, spec, current *agentapi.Spec) *agentapi.Spec {
	if spec == nil || !spec.Revert {
		return spec
	}
	if saved, err := a.readRestoredProject(key); err == nil && saved.Spec != nil &&
		saved.Spec.InstanceID == spec.InstanceID && saved.Spec.Package == spec.Package {
		resolved := *saved.Spec
		resolved.Generation = spec.Generation
		return &resolved
	}
	if current != nil && current.Package == spec.Package {
		resolved := *current
		resolved.Generation = spec.Generation
		return &resolved
	}
	return spec
}
