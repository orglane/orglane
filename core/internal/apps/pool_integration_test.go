//go:build integration

package apps_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/providers/provider"
	"github.com/orglane/orglane/providers/testprovider"
)

// Regression tests for #82: a job holding a session advisory lock does all
// its database work on that connection, so it never waits for a second one
// (database.max_conns=1 is a supported setting).

// credentialReading is the test provider with provisioning calls that read
// a credential through Env first, as real adapters do for their API token.
type credentialReading struct{ *testprovider.Provider }

func (p credentialReading) read(ctx context.Context, env provider.Env) error {
	if _, err := env.Credential(ctx, "api_token"); err != nil && !errors.Is(err, provider.ErrNotFound) {
		return err
	}
	return nil
}

func (p credentialReading) Plan(ctx context.Context, env provider.Env, inst provider.Instance, d provider.DesiredAccess) (provider.Plan, error) {
	if err := p.read(ctx, env); err != nil {
		return provider.Plan{}, err
	}
	return p.Provider.Plan(ctx, env, inst, d)
}

func (p credentialReading) Apply(ctx context.Context, env provider.Env, inst provider.Instance, a provider.Action) (provider.ActionOutcome, error) {
	if err := p.read(ctx, env); err != nil {
		return provider.ActionOutcome{}, err
	}
	return p.Provider.Apply(ctx, env, inst, a)
}

func (p credentialReading) SyncTeams(ctx context.Context, env provider.Env, inst provider.Instance, teams []provider.TeamMapping) ([]provider.TeamRef, error) {
	if err := p.read(ctx, env); err != nil {
		return nil, err
	}
	return p.Provider.SyncTeams(ctx, env, inst, teams)
}

func (p credentialReading) Members(ctx context.Context, env provider.Env, inst provider.Instance) ([]provider.ExternalAccount, error) {
	if err := p.read(ctx, env); err != nil {
		return nil, err
	}
	return p.Provider.Members(ctx, env, inst)
}

// credentialReadingService is a service on pool whose test provider reads
// credentials in every provisioning call.
func credentialReadingService(t *testing.T, h *harness, pool *pgxpool.Pool) *apps.Service {
	t.Helper()
	reg, err := apps.NewRegistry(credentialReading{h.tp})
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	svc := apps.NewService(pool, reg, ring, apps.Config{Logger: h.cfg.Logger})
	rc, err := river.NewClient(riverpgxv5.New(pool), &river.Config{})
	if err != nil {
		t.Fatal(err)
	}
	svc.SetQueue(rc)
	return svc
}

// TestProvisioningMinimumPool: provision_instance, provision_person and the
// removal guard (which takes the person's lock under its own) finish on a
// one-connection pool, with adapters reading credentials under the lock.
func TestProvisioningMinimumPool(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	policy := provider.MappedTeams
	if _, err := h.svc.UpdateInstance(h.ctx, audit.System, inst.ID, apps.InstanceUpdate{AccessPolicy: &policy}); err != nil {
		t.Fatal(err)
	}
	alice, bob := h.person("alice"), h.person("bob")
	h.mapTeam(inst, h.team("core", alice))
	leaving := h.team("leaving", bob)
	h.mapTeam(inst, leaving)
	h.drain()

	svc := credentialReadingService(t, h, sizedPool(t, h, 1))
	if err := svc.RunProvisionInstance(bounded(t), apps.ProvisionInstanceArgs{InstanceID: inst.ID}); err != nil {
		t.Fatalf("provision_instance with max_conns=1: %v", err)
	}
	if err := svc.RunProvisionPerson(bounded(t), apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: alice.ID, Retry: true}, 1, 8); err != nil {
		t.Fatalf("provision_person with max_conns=1: %v", err)
	}

	// bob leaves: his run plans removals, the guard applies them under his
	// lock.
	if err := h.ids.RemoveTeamMember(h.ctx, audit.System, leaving.ID, mustTeamMember(h, leaving)); err != nil {
		t.Fatal(err)
	}
	if err := svc.RunProvisionPerson(bounded(t), apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: bob.ID}, 1, 8); err != nil {
		t.Fatalf("planning removals with max_conns=1: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, bob.ID); n == 0 {
		t.Fatal("no removal was planned")
	}
	if err := svc.RunApplyRemovals(bounded(t), apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatalf("apply_removals with max_conns=1: %v", err)
	}
	if acct, _ := h.tp.Account(inst.ID, "bob"); acct.Member {
		t.Fatal("bob kept access")
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, bob.ID); n == 0 {
		t.Fatal("no removal was applied")
	}
	// The nested person lock was released with the run: bob's own job
	// takes it.
	if err := svc.RunProvisionPerson(bounded(t), apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: bob.ID}, 1, 8); err != nil {
		t.Fatalf("provision_person after the guard: %v", err)
	}
	if n := h.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted`); n != 0 {
		t.Fatalf("%d advisory locks still held", n)
	}
}

// TestProvisioningMinimumPoolConcurrent: concurrent workers of every
// provisioning job on a one-connection pool all finish: a lock holder never
// waits for the pool, so the connection always comes back.
func TestProvisioningMinimumPoolConcurrent(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	people := []uuid.UUID{h.person("alice").ID, h.person("bob").ID, h.person("carol").ID}
	h.drain()
	// A removal for the guard to work on: carol is suspended and her run
	// plans the removal of her account.
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, people[2], identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: people[2]}, 1, 8); err != nil {
		t.Fatal(err)
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'pending'`, people[2]); n == 0 {
		t.Fatal("no removal was planned")
	}

	svc := credentialReadingService(t, h, sizedPool(t, h, 1))
	ctx := bounded(t)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for range 3 {
		wg.Go(func() { errs <- svc.RunProvisionInstance(ctx, apps.ProvisionInstanceArgs{InstanceID: inst.ID}) })
		wg.Go(func() { errs <- svc.RunApplyRemovals(ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8) })
		for _, id := range people {
			wg.Go(func() {
				errs <- svc.RunProvisionPerson(ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: id}, 1, 8)
			})
		}
	}
	wg.Wait()
	close(errs)
	if ctx.Err() != nil {
		t.Fatalf("workers waited on the pool: %v", ctx.Err())
	}
	for err := range errs {
		// A follow-up snooze is a normal outcome of concurrent runs.
		var snooze *river.JobSnoozeError
		if err != nil && !errors.As(err, &snooze) {
			t.Fatalf("worker failed: %v", err)
		}
	}
	if n := h.count(`SELECT count(*) FROM provisioning_removals WHERE person_id = $1 AND state = 'applied'`, people[2]); n == 0 {
		t.Fatal("the guard applied no removal")
	}
	if acct, _ := h.tp.Account(inst.ID, "carol"); acct.Member {
		t.Fatal("carol kept access")
	}
}

// TestRemovalGuardCanceledPersonLock: when the guard's wait for a person's
// lock is canceled, its connection is discarded rather than returned to the
// pool with the instance's removals lock still held.
func TestRemovalGuardCanceledPersonLock(t *testing.T) {
	h := newHarness(t)
	inst := h.instance(nil)
	carol := h.person("carol")
	h.drain()
	if _, err := h.ids.SetPersonStatus(h.ctx, audit.System, carol.ID, identity.StatusSuspended); err != nil {
		t.Fatal(err)
	}
	if err := h.svc.RunProvisionPerson(h.ctx, apps.ProvisionPersonArgs{InstanceID: inst.ID, PersonID: carol.ID}, 1, 8); err != nil {
		t.Fatal(err)
	}

	// Another session holds carol's provisioning lock, so the guard waits
	// for it under the removals lock until its context ends.
	person := apps.LockIDForTest("provision", inst.ID.String(), carol.ID.String())
	blocker, err := h.pool.Acquire(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err := blocker.Exec(h.ctx, "SELECT pg_advisory_lock($1)", person); err != nil {
		t.Fatal(err)
	}
	svc := credentialReadingService(t, h, sizedPool(t, h, 1))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- svc.RunApplyRemovals(ctx, apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8) }()

	// The guard's session holds the removals lock and waits for carol's.
	removals := apps.LockIDForTest("removals", inst.ID.String())
	wait := bounded(t)
	for {
		var waiting bool
		if err := blocker.QueryRow(wait, `SELECT EXISTS (SELECT 1 FROM pg_locks w JOIN pg_locks g ON g.pid = w.pid
			WHERE w.locktype = 'advisory' AND NOT w.granted AND ((w.classid::bigint << 32) | w.objid::bigint) = $1
			AND g.locktype = 'advisory' AND g.granted AND ((g.classid::bigint << 32) | g.objid::bigint) = $2)`, person, removals).Scan(&waiting); err != nil {
			t.Fatalf("the guard never waited for the person's lock: %v", err)
		}
		if waiting {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		// The canceled wait fails the run; which error (the context's or a
		// closed connection) depends on where the cancellation lands.
		if err == nil {
			t.Fatal("the guard applied a removal while the person's lock was held")
		}
	case <-wait.Done():
		t.Fatal("the guard did not stop after its context was canceled")
	}
	if _, err := blocker.Exec(h.ctx, "SELECT pg_advisory_unlock($1)", person); err != nil {
		t.Fatal(err)
	}

	// A separate session gets the removals lock once the discarded backend
	// is gone.
	for {
		var ok bool
		if err := blocker.QueryRow(wait, "SELECT pg_try_advisory_lock($1)", removals).Scan(&ok); err != nil {
			t.Fatalf("the removals lock stayed held: %v", err)
		}
		if ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := blocker.Exec(h.ctx, "SELECT pg_advisory_unlock($1)", removals); err != nil {
		t.Fatal(err)
	}
	// The one-connection pool works again and the guard finishes.
	if err := svc.RunApplyRemovals(bounded(t), apps.ApplyRemovalsArgs{InstanceID: inst.ID}, 1, 8); err != nil {
		t.Fatalf("apply_removals after the canceled run: %v", err)
	}
	if acct, _ := h.tp.Account(inst.ID, "carol"); acct.Member {
		t.Fatal("carol kept access")
	}
	if n := h.count(`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted`); n != 0 {
		t.Fatalf("%d advisory locks still held", n)
	}
}
