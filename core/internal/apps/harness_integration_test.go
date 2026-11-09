//go:build integration

package apps_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/database"
	"github.com/orglane/orglane/core/internal/dbtest"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
	"github.com/orglane/orglane/tests/pgtest"
)

func TestMain(m *testing.M) { dbtest.Main(m) }

const masterSecret = "integration-test-master-secret-0123456789"

var fastHasher = crypto.NewPasswordHasher(crypto.PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}, 4)

// harness is the provider framework on a fresh database with the test
// provider (connected) and its managed variant registered. Jobs are queued
// by an insert-only River client and worked by drain, deterministically.
type harness struct {
	t       *testing.T
	ctx     context.Context
	pool    *pgxpool.Pool
	ids     *identity.Service
	svc     *apps.Service
	tp      *testprovider.Provider
	managed *testprovider.Provider
	// agentProv is the managed variant whose package is agenttest.
	agentProv *testprovider.Provider
	reg       *apps.Registry
	keyring   *crypto.Keyring
	cfg       apps.Config
	queue     apps.Queue
}

// newHarness builds the harness; opts change the service's configuration
// (agent client, packages, intervals).
func newHarness(t *testing.T, opts ...func(*apps.Config)) *harness {
	t.Helper()
	var cfg apps.Config
	for _, o := range opts {
		o(&cfg)
	}
	return newHarnessWith(t, cfg)
}

// newHarnessWith is newHarness with more of the service's configuration
// (WorkspaceURL and Logger are set here) and further adapters.
func newHarnessWith(t *testing.T, cfg apps.Config, adapters ...provider.CapabilityManifest) *harness {
	t.Helper()
	ctx := context.Background()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	pool, err := database.Connect(ctx, config.Database{URL: pgtest.NewDatabase(t), MaxConns: 10, ConnectTimeout: 10 * time.Second}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	m, err := database.NewMigrator(pool, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = m.Close()
	keyring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	tp := testprovider.New()
	managed := testprovider.New(testprovider.WithID("testmanaged"), testprovider.Managed())
	agentProv := testprovider.New(testprovider.WithID("agenttest"), testprovider.Managed())
	reg, err := apps.NewRegistry(append([]provider.CapabilityManifest{tp, managed, agentProv}, adapters...)...)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WorkspaceURL == nil {
		cfg.WorkspaceURL, _ = url.Parse("https://work.acme.test")
	}
	cfg.Logger = logger
	// Local mode for managed services (http://*.orglane.localhost).
	ids := identity.NewService(pool, identity.WithPasswordHasher(fastHasher), identity.WithLocalServiceDomain("orglane.localhost"))
	if cfg.OIDC == nil {
		cfg.OIDC = ids
	}
	svc := apps.NewService(pool, reg, keyring, cfg)
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetQueue(rc)
	ids.SetDirectoryObserver(svc)
	if err := ids.SeedBuiltinRoles(ctx); err != nil {
		t.Fatal(err)
	}
	return &harness{t: t, ctx: ctx, pool: pool, ids: ids, svc: svc, tp: tp, managed: managed, agentProv: agentProv, reg: reg, keyring: keyring, cfg: cfg, queue: rc}
}

// restartCore returns a new service on the same database, as after a core
// restart: nothing in memory survives.
func (h *harness) restartCore(opts ...func(*apps.Config)) *apps.Service {
	cfg := h.cfg
	for _, o := range opts {
		o(&cfg)
	}
	svc := apps.NewService(h.pool, h.reg, h.keyring, cfg)
	svc.SetQueue(h.queue)
	return svc
}

// instance creates an active test provider instance.
func (h *harness) instance(settings map[string]any) apps.Instance {
	h.t.Helper()
	inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{Provider: testprovider.ID, State: apps.StateActive, BaseURL: "https://test.example", Settings: settings})
	if err != nil {
		h.t.Fatal(err)
	}
	return inst
}

func (h *harness) person(username string) store.Person {
	h.t.Helper()
	p, err := h.ids.CreatePerson(h.ctx, audit.System, identity.NewPerson{Username: username, WorkEmail: username + "@acme.test", DisplayName: "Person " + username, Status: identity.StatusActive})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) team(slug string, members ...store.Person) store.Team {
	h.t.Helper()
	team, err := h.ids.CreateTeam(h.ctx, audit.System, identity.NewTeam{Slug: slug, Name: "Team " + slug})
	if err != nil {
		h.t.Fatal(err)
	}
	for _, m := range members {
		if _, err := h.ids.SetTeamMember(h.ctx, audit.System, team.ID, m.ID, identity.TeamRoleMember); err != nil {
			h.t.Fatal(err)
		}
	}
	return team
}

type queued struct {
	id          int64
	kind        string
	args        []byte
	attempt     int
	maxAttempts int
}

// drain works queued jobs until none is left, ignoring their schedule.
// Errors count as attempts (retried until exhausted), snoozes rerun.
func (h *harness) drain() {
	h.t.Helper()
	runs := map[int64]int{}
	for i := 0; ; i++ {
		if i > 2000 {
			h.t.Fatal("drain: jobs do not settle")
		}
		var j queued
		err := h.pool.QueryRow(h.ctx, `SELECT id, kind, args, attempt, max_attempts FROM river_job
			WHERE state IN ('available', 'scheduled', 'retryable') ORDER BY id LIMIT 1`).Scan(&j.id, &j.kind, &j.args, &j.attempt, &j.maxAttempts)
		if errors.Is(err, pgx.ErrNoRows) {
			return
		} else if err != nil {
			h.t.Fatal(err)
		}
		runs[j.id]++
		if runs[j.id] > 30 {
			h.t.Fatalf("drain: job %d (%s) keeps running", j.id, j.kind)
		}
		err = h.work(j)
		var snooze *river.JobSnoozeError
		var cancel *river.JobCancelError
		switch {
		case err == nil, errors.As(err, &cancel):
			h.exec(`DELETE FROM river_job WHERE id = $1`, j.id)
		case errors.As(err, &snooze):
			// rerun
		case j.attempt+1 >= j.maxAttempts:
			h.exec(`DELETE FROM river_job WHERE id = $1`, j.id)
		default:
			h.exec(`UPDATE river_job SET attempt = attempt + 1, state = 'retryable' WHERE id = $1`, j.id)
		}
	}
}

func (h *harness) work(j queued) error {
	attempt := j.attempt + 1
	switch j.kind {
	case apps.ProvisionPersonArgs{}.Kind():
		var a apps.ProvisionPersonArgs
		h.decode(j.args, &a)
		return h.svc.RunProvisionPerson(h.ctx, a, attempt, j.maxAttempts)
	case apps.ProvisionInstanceArgs{}.Kind():
		var a apps.ProvisionInstanceArgs
		h.decode(j.args, &a)
		return h.svc.RunProvisionInstance(h.ctx, a)
	case apps.ApplyRemovalsArgs{}.Kind():
		var a apps.ApplyRemovalsArgs
		h.decode(j.args, &a)
		return h.svc.RunApplyRemovals(h.ctx, a, attempt, j.maxAttempts)
	case apps.ProcessDeliveryArgs{}.Kind():
		var a apps.ProcessDeliveryArgs
		h.decode(j.args, &a)
		return h.svc.RunProcessDelivery(h.ctx, a)
	case apps.RotateCredentialArgs{}.Kind():
		var a apps.RotateCredentialArgs
		h.decode(j.args, &a)
		return h.svc.RunRotateCredential(h.ctx, a)
	case apps.SetupArgs{}.Kind():
		var a apps.SetupArgs
		h.decode(j.args, &a)
		return h.svc.RunSetup(h.ctx, a, attempt, j.maxAttempts)
	case apps.EnsureWebhooksArgs{}.Kind():
		var a apps.EnsureWebhooksArgs
		h.decode(j.args, &a)
		return h.svc.RunEnsureWebhooks(h.ctx, a)
	case apps.DriftCheckArgs{}.Kind():
		return h.svc.RunDriftCheck(h.ctx)
	case apps.CleanupArgs{}.Kind():
		_, err := h.svc.Cleanup(h.ctx)
		return err
	case apps.AgentApplyArgs{}.Kind():
		var a apps.AgentApplyArgs
		h.decode(j.args, &a)
		return h.svc.RunAgentApply(h.ctx, a)
	case apps.AgentSyncArgs{}.Kind():
		return h.svc.RunAgentSync(h.ctx)
	}
	h.t.Fatalf("unknown job kind %s", j.kind)
	return nil
}

func (h *harness) decode(raw []byte, v any) {
	h.t.Helper()
	if err := json.Unmarshal(raw, v); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) exec(sql string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(h.ctx, sql, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) count(sql string, args ...any) int {
	h.t.Helper()
	var n int
	if err := h.pool.QueryRow(h.ctx, sql, args...).Scan(&n); err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) jobs(kind string) int {
	return h.count(`SELECT count(*) FROM river_job WHERE kind = $1 AND state IN ('available', 'scheduled', 'retryable')`, kind)
}

func (h *harness) result(inst apps.Instance, p store.Person) store.ProvisioningResult {
	h.t.Helper()
	r, err := store.New(h.pool).GetProvisioningResult(h.ctx, store.GetProvisioningResultParams{InstanceID: inst.ID, PersonID: p.ID})
	if err != nil {
		h.t.Fatalf("result of %s: %v", p.Username, err)
	}
	return r
}

func (h *harness) actions(r store.ProvisioningResult) []apps.ActionResult {
	h.t.Helper()
	var out []apps.ActionResult
	h.decode(r.Actions, &out)
	return out
}

func (h *harness) auditCount(action audit.Action) int {
	return h.count(`SELECT count(*) FROM audit_entries WHERE action = $1`, string(action))
}

// mapTeam maps a team in create mode and syncs it.
func (h *harness) mapTeam(inst apps.Instance, team store.Team) {
	h.t.Helper()
	if _, err := h.svc.SetTeamMapping(h.ctx, audit.System, inst.ID, team.ID, apps.TeamMappingInput{Mode: provider.MappingCreate}); err != nil {
		h.t.Fatal(err)
	}
}

func retryable(code string) error { return &provider.Error{Code: code, Retryable: true} }

func permanent(code string) error { return &provider.Error{Code: code} }

func newID() uuid.UUID { return uuid.Must(uuid.NewV7()) }
