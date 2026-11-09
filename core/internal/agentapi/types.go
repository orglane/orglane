// Package agentapi holds the wire types and the request and response
// signatures shared by the agent (core/internal/agent) and the core's client
// (core/internal/agentclient). The protocol is specified in
// docs/specs/agent.md and decided in ADR 0019.
package agentapi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"time"

	"github.com/google/uuid"
)

// ProtocolVersion is the agent protocol version reported by GET /v1/health.
const ProtocolVersion = 1

// MaxRequestBytes is the largest request body the agent accepts.
const MaxRequestBytes = 1 << 20

// MaxLogBytes is the largest log response the agent sends.
const MaxLogBytes = 1 << 20

// MaxLogTail is the largest number of log lines per service the agent returns.
const MaxLogTail = 1000

// Paths of the agent API. There is no endpoint that takes a command, an
// image, a Compose file or a path (ADR 0019).
const (
	PathHealth    = "/v1/health"
	PathInstances = "/v1/instances"
)

// InstancePath returns the path of one instance.
func InstancePath(key string) string { return PathInstances + "/" + url.PathEscape(key) }

// InstanceLogsPath returns the path of an instance's logs.
func InstanceLogsPath(key string) string { return InstancePath(key) + "/logs" }

// InstanceSnapshotsPath returns the path of an instance's snapshots.
func InstanceSnapshotsPath(key string) string { return InstancePath(key) + "/snapshots" }

// InstanceRestorePath returns the path that restores an instance's snapshot.
func InstanceRestorePath(key string) string { return InstancePath(key) + "/restore" }

// OperationPath returns the path of an operation.
func OperationPath(id uuid.UUID) string { return "/v1/operations/" + id.String() }

// OperationConfirmPath returns the path that confirms an upgrade.
func OperationConfirmPath(id uuid.UUID) string { return OperationPath(id) + "/confirm" }

// OperationAbortPath returns the path that rolls an upgrade back.
func OperationAbortPath(id uuid.UUID) string { return OperationPath(id) + "/abort" }

// KeyPattern is the pattern of instance keys (providers.md "app_instances").
// Keys name the instance directory, the Compose project and the database.
var KeyPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,22}[a-z0-9]$`)

// Run states of a desired spec.
const (
	StateRunning = "running"
	StateStopped = "stopped"
	StateAbsent  = "absent"
)

// Ingress modes (config.IngressDomain, config.IngressLocal).
const (
	IngressLocal  = "local"
	IngressDomain = "domain"
)

// PackageRef names an embedded managed package version.
type PackageRef struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (p PackageRef) String() string { return p.ID + "@" + p.Version }

// URLs are the absolute URLs a managed service needs.
type URLs struct {
	Workspace string `json:"workspace"`
	Issuer    string `json:"issuer"`
	Instance  string `json:"instance"`
	Webhook   string `json:"webhook"`
}

// Spec is the desired state of one app instance, pushed by the core
// (docs/specs/agent.md "Desired spec"). It carries only typed inputs that the
// package manifest declares; the agent renders the Compose project itself.
type Spec struct {
	InstanceID uuid.UUID `json:"instance_id"`
	// Generation increases strictly per instance.
	Generation int64      `json:"generation"`
	Package    PackageRef `json:"package"`
	// State is StateRunning, StateStopped or StateAbsent.
	State string `json:"state"`
	// Purge, with StateAbsent, deletes the data, the database and the role.
	Purge bool `json:"purge,omitempty"`
	// Hosts maps route names to public hostnames, plus "workspace".
	Hosts map[string]string `json:"hosts"`
	URLs  URLs              `json:"urls"`
	// Ingress is IngressLocal or IngressDomain.
	Ingress string `json:"ingress_mode"`
	// Settings are the manifest-declared settings.
	Settings map[string]any `json:"settings"`
	// Secrets maps manifest-declared secret names to their values.
	Secrets map[string]string `json:"secrets"`
	// Revert marks the core's acknowledgment of a rollback whose restored
	// version this release no longer embeds (docs/specs/agent.md "Upgrade").
	// Package must name the version that runs; the agent resolves the spec
	// from the running project (current.json) and records this generation.
	// restored-project.json only supplies rendered files when its spec
	// matches the running one. It carries no inputs.
	Revert bool `json:"revert,omitempty"`
}

// Hash returns the canonical hash of the spec: SHA-256 of its JSON encoding
// (encoding/json sorts map keys, so equal specs hash equally).
func (s *Spec) Hash() string {
	b, err := json.Marshal(s)
	if err != nil {
		// Only possible for unsupported setting values, which decoding
		// from JSON never produces.
		panic(fmt.Sprintf("agentapi: marshal spec: %v", err))
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// SecretValues returns the secret values of the spec (for redaction).
func (s *Spec) SecretValues() []string {
	out := make([]string, 0, len(s.Secrets))
	for _, v := range s.Secrets {
		if v != "" {
			out = append(out, v)
		}
	}
	slices.Sort(out)
	return out
}

// Operation kinds.
const (
	KindApply    = "apply"
	KindUpgrade  = "upgrade"
	KindSnapshot = "snapshot"
	KindRestore  = "restore"
	KindRemove   = "remove"
)

// Operation states.
const (
	OpQueued               = "queued"
	OpRunning              = "running"
	OpAwaitingConfirmation = "awaiting_confirmation"
	OpSucceeded            = "succeeded"
	OpFailed               = "failed"
	OpRolledBack           = "rolled_back"
)

// Operation phases of apply and remove operations.
const (
	PhaseRendering   = "rendering"
	PhasePolicyCheck = "policy_check"
	PhasePreparing   = "preparing"
	PhaseDatabase    = "database"
	PhaseStarting    = "starting"
	PhaseConfiguring = "configuring"
	PhaseStopping    = "stopping"
	PhasePurging     = "purging"
	// Remove operations use PhaseRemoving (the instance phase) too.
)

// Phases of upgrade operations and of the upgrade journal
// (docs/specs/agent.md "Upgrade"). Upgrades also use PhaseStopping, and
// PhaseRollingBack (the instance phase) for the fenced restore. Restore
// operations use PhaseRollingBack and PhaseRestored; snapshot operations
// PhaseStopping, PhaseSnapshotting and PhaseStarting.
const (
	PhasePreflight            = "preflight"
	PhasePulling              = "pulling"
	PhaseSnapshotting         = "snapshotting"
	PhaseSwitching            = "switching"
	PhaseMigrating            = "migrating"
	PhaseVerifying            = "verifying"
	PhaseAwaitingConfirmation = "awaiting_confirmation"
	PhaseCommitting           = "committing"
	PhaseRestored             = "restored"
)

// Error codes of problem details and operations (docs/specs/agent.md
// "Operations"). NotFound and Superseded are additions of the agent: the
// first answers unknown instances and operations, the second marks a queued
// operation whose spec was replaced by a newer generation before it ran.
const (
	CodeUnauthenticated       = "unauthenticated"
	CodeAgentBusy             = "agent_busy"
	CodeInstanceMismatch      = "instance_mismatch"
	CodeStaleGeneration       = "stale_generation"
	CodeGenerationConflict    = "generation_conflict"
	CodeUnknownPackageVersion = "unknown_package_version"
	CodeInvalidInput          = "invalid_input"
	CodePolicyViolation       = "policy_violation"
	CodeUpgradePathBlocked    = "upgrade_path_blocked"
	CodeInsufficientDisk      = "insufficient_disk"
	CodePullFailed            = "pull_failed"
	CodeComposeFailed         = "compose_failed"
	CodeDatabaseFailed        = "database_failed"
	CodeHookFailed            = "hook_failed"
	CodeHealthTimeout         = "health_timeout"
	CodeConfirmTimeout        = "confirm_timeout"
	CodeAborted               = "aborted"
	CodeSnapshotFailed        = "snapshot_failed"
	CodeRollbackFailed        = "rollback_failed"
	CodeInterrupted           = "interrupted"
	CodeNotFound              = "not_found"
	CodeSuperseded            = "superseded"
	CodeInternal              = "internal"
	// CodeNotAwaitingConfirmation answers confirm or abort for an operation
	// that is not an upgrade waiting for its confirmation (409).
	CodeNotAwaitingConfirmation = "not_awaiting_confirmation"
)

// Snapshot reasons.
const (
	SnapshotReasonUpgrade = "upgrade"
	SnapshotReasonManual  = "manual"
)

// SnapshotFormat is the format of snapshot.json.
const SnapshotFormat = 1

// SnapshotRequest is the body of POST /v1/instances/{key}/snapshots.
type SnapshotRequest struct {
	// Reason is SnapshotReasonManual; upgrade snapshots are the agent's own.
	Reason string `json:"reason"`
}

// RestoreRequest is the body of POST /v1/instances/{key}/restore.
type RestoreRequest struct {
	SnapshotID uuid.UUID `json:"snapshot_id"`
	// Generation is the new generation of the restored state: the spec of
	// the snapshot becomes the accepted and, when the restore succeeded, the
	// running spec with this generation. It must be newer than the accepted
	// generation.
	Generation int64 `json:"generation"`
}

// SnapshotFile is a file of a snapshot with its size and SHA-256.
type SnapshotFile struct {
	File   string `json:"file"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

// SnapshotDataDir is the archive of one data directory.
type SnapshotDataDir struct {
	Name string `json:"name"`
	SnapshotFile
}

// Snapshot describes a complete snapshot: snapshot.json, and the answer of
// GET /v1/instances/{key}/snapshots (docs/specs/agent.md "Snapshots"). It
// carries no secrets; the spec at snapshot time stays in the agent.
type Snapshot struct {
	Format     int               `json:"format"`
	ID         uuid.UUID         `json:"id"`
	CreatedAt  time.Time         `json:"created_at"`
	Reason     string            `json:"reason"`
	Package    PackageRef        `json:"package"`
	Generation int64             `json:"generation"`
	Images     map[string]string `json:"images"`
	Database   *SnapshotFile     `json:"database,omitempty"`
	DataDirs   []SnapshotDataDir `json:"data_dirs"`
	// RenderedSHA256 is a hash over the rendered files of the snapshot.
	RenderedSHA256 string `json:"rendered_sha256"`
}

// SnapshotList is the answer of GET /v1/instances/{key}/snapshots, newest
// first.
type SnapshotList struct {
	Snapshots []Snapshot `json:"snapshots"`
}

// Operation is one declarative operation of the agent.
type Operation struct {
	ID         uuid.UUID `json:"id"`
	Instance   string    `json:"instance"`
	Kind       string    `json:"kind"`
	Generation int64     `json:"generation,omitempty"`
	State      string    `json:"state"`
	Phase      string    `json:"phase,omitempty"`
	ErrorCode  string    `json:"error_code,omitempty"`
	Rule       string    `json:"rule,omitempty"` // the policy rule, with CodePolicyViolation
	SnapshotID string    `json:"snapshot_id,omitempty"`
	StartedAt  time.Time `json:"started_at"`
	// ConfirmBy is the confirmation deadline of an upgrade in
	// awaiting_confirmation (the agent's clock). The core checks its
	// candidate before it and never confirms after it.
	ConfirmBy  *time.Time `json:"confirm_by,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Terminal reports whether the operation has finished.
func (o *Operation) Terminal() bool {
	switch o.State {
	case OpSucceeded, OpFailed, OpRolledBack:
		return true
	}
	return false
}

// Instance phases reported by the agent.
const (
	PhasePending     = "pending"
	PhaseInstalling  = "installing"
	PhaseRunning     = "running"
	PhaseDegraded    = "degraded"
	PhaseStopped     = "stopped"
	PhaseUpgrading   = "upgrading"
	PhaseRollingBack = "rolling_back"
	PhaseFailed      = "failed"
	PhaseRemoving    = "removing"
	PhaseAbsent      = "absent"
)

// ServiceStatus is the state of one Compose service of an instance.
type ServiceStatus struct {
	Name   string `json:"name"`
	State  string `json:"state"`            // Docker container state: running, exited, ...
	Health string `json:"health,omitempty"` // healthy, unhealthy, starting
}

// InstanceStatus is what the agent reports for one instance.
type InstanceStatus struct {
	Key                string          `json:"key"`
	InstanceID         uuid.UUID       `json:"instance_id"`
	Phase              string          `json:"phase"`
	AcceptedGeneration int64           `json:"accepted_generation"`
	ObservedGeneration int64           `json:"observed_generation"`
	Package            *PackageRef     `json:"package,omitempty"` // the running package version
	State              string          `json:"state,omitempty"`   // the run state of the running spec
	Services           []ServiceStatus `json:"services"`
	Operation          *Operation      `json:"operation,omitempty"` // the newest operation
	BlockedGenerations []int64         `json:"blocked_generations,omitempty"`
	ErrorCode          string          `json:"error_code,omitempty"` // of the last failed reconcile
	Snapshots          int             `json:"snapshots"`
	DataRetained       bool            `json:"data_retained"`
}

// InstanceList is the answer of GET /v1/instances.
type InstanceList struct {
	Instances []InstanceStatus `json:"instances"`
}

// Health is the answer of GET /v1/health.
type Health struct {
	Protocol     int          `json:"protocol"`
	Version      string       `json:"version"`
	BootID       uuid.UUID    `json:"boot_id"`
	Packages     []PackageRef `json:"packages"`
	MemTotal     uint64       `json:"mem_total"`
	MemAvailable uint64       `json:"mem_available"`
	DiskFree     uint64       `json:"disk_free"`
}

// Problem is an RFC 9457 problem details object with a stable code.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
	Code     string `json:"code"`
	// AcceptedGeneration is set with CodeStaleGeneration.
	AcceptedGeneration int64 `json:"accepted_generation,omitempty"`
	// Fields names the invalid inputs with CodeInvalidInput (never values).
	Fields []string `json:"fields,omitempty"`
	// RequiredVersion is set with CodeUpgradePathBlocked when a stop exists.
	RequiredVersion string `json:"required_version,omitempty"`
}

// ProblemContentType is the media type of problem details.
const ProblemContentType = "application/problem+json"

// packageIDPattern is the pattern of package and provider IDs.
var packageIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

// Check validates the shape of a spec that does not depend on its package
// and returns the names of invalid fields. The package's manifest checks the
// inputs (packages.Package.Inputs).
func (s *Spec) Check() []string {
	var fields []string
	if s.InstanceID == uuid.Nil {
		fields = append(fields, "instance_id")
	}
	if s.Generation <= 0 {
		fields = append(fields, "generation")
	}
	if !packageIDPattern.MatchString(s.Package.ID) {
		fields = append(fields, "package.id")
	}
	if s.Package.Version == "" || len(s.Package.Version) > 64 {
		fields = append(fields, "package.version")
	}
	switch s.State {
	case StateRunning, StateStopped:
		if s.Purge {
			fields = append(fields, "purge")
		}
	case StateAbsent:
	default:
		fields = append(fields, "state")
	}
	if s.Revert && s.State != StateRunning {
		fields = append(fields, "state")
	}
	if s.Ingress != IngressLocal && s.Ingress != IngressDomain {
		fields = append(fields, "ingress_mode")
	}
	return fields
}
