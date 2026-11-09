//go:build integration

package apps_test

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/ingress"
	"github.com/orglane/orglane/core/internal/packages"
	agenttest "github.com/orglane/orglane/providers/agenttest/managed"
)

// TestIngressRoutes: managed instances get their package's route once the
// agent applied their generation, keep it while active or degraded, point
// it at the core's maintenance answer while upgrading, and lose it when
// they fail or are removed. Connected instances never get routes.
func TestIngressRoutes(t *testing.T) {
	pkgs, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarnessWith(t, apps.Config{Packages: pkgs})
	h.instance(nil) // connected

	type row struct {
		key, state        string
		desired, observed int64
		version           *string
	}
	running := agenttest.Version
	rows := []row{
		{"pending", apps.StateInstalling, 1, 0, nil},        // the agent has not applied it yet
		{"applied", apps.StateInstalling, 1, 1, nil},        // applied: Setup can use the public name
		{"reinstall", apps.StateInstalling, 3, 2, &running}, // a new generation is not applied yet
		{"active", apps.StateActive, 2, 2, &running},
		{"degraded", apps.StateDegraded, 2, 2, &running},
		{"upgrading", apps.StateUpgrading, 3, 2, &running},
		{"failed", apps.StateFailed, 1, 1, &running},
		{"removed", apps.StateRemoved, 2, 2, &running},
		{"nopkg", apps.StateActive, 1, 1, nil}, // its package is not embedded
	}
	for _, r := range rows {
		inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
			Provider: "testmanaged", Key: r.key, BaseURL: "https://" + r.key + ".acme.test",
		})
		if err != nil {
			t.Fatal(err)
		}
		pkg := "agenttest"
		if r.key == "nopkg" {
			pkg = "missing"
		}
		if _, err := h.pool.Exec(h.ctx, `UPDATE app_instances SET package_id = $6, package_version = $2,
			desired_generation = $3, observed_generation = $4, state = $5 WHERE id = $1`,
			inst.ID, r.version, r.desired, r.observed, r.state, pkg); err != nil {
			t.Fatal(err)
		}
	}

	got, err := h.svc.IngressRoutes(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	slices.SortFunc(got, func(a, b ingress.AppRoute) int { return strings.Compare(a.Key, b.Key) })
	want := []ingress.AppRoute{
		{Key: "active", Route: "web", Host: "active.acme.test", Upstream: "http://active-web:8080"},
		{Key: "applied", Route: "web", Host: "applied.acme.test", Upstream: "http://applied-web:8080"},
		{Key: "degraded", Route: "web", Host: "degraded.acme.test", Upstream: "http://degraded-web:8080"},
		{Key: "upgrading", Route: "web", Host: "upgrading.acme.test", Maintenance: true},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("routes =\n%+v\nwant\n%+v", got, want)
	}
}

// TestActivationWaitsForPublishedRoute: with Config.AwaitRoutes, the setup
// job activates an installing instance only after the internal handler has
// served Traefik a configuration with its route and the settle time has
// passed (docs/specs/providers.md "Route publication"). The evidence is in
// the database: here one core process answers Traefik's polls and another
// runs the setup job. An unpublished route fails the attempt (retryable),
// exhausted attempts fail the instance with setup_failed, and a reinstall
// waits for its route again.
func TestActivationWaitsForPublishedRoute(t *testing.T) {
	pkgs, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	const settle, wait = 300 * time.Millisecond, 2 * time.Second
	h := newHarnessWith(t, apps.Config{Packages: pkgs, AwaitRoutes: true, RouteSettle: settle, RouteWait: wait})
	vars := map[string]string{
		"ORGLANE_DATABASE_URL":           "postgres://unused",
		"ORGLANE_DOMAIN_BASE":            "acme.test",
		"ORGLANE_DOMAIN_WORKSPACE_HOST":  "work.acme.test",
		"ORGLANE_ENVIRONMENT":            "development",
		"ORGLANE_IDENTITY_MASTER_SECRET": masterSecret,
	}
	cfg, err := config.Load("", func(k string) (string, bool) { v, ok := vars[k]; return v, ok })
	if err != nil {
		t.Fatal(err)
	}
	// The process Traefik polls, and another core process on the same
	// database that runs the setup jobs.
	poller, worker := h.svc, h.restartCore()
	handler, err := ingress.NewHandler(cfg, poller.IngressRoutes, poller.PublishRoutes, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	// poll is one poll of Traefik's HTTP provider.
	poll := func() {
		t.Helper()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequestWithContext(h.ctx, http.MethodGet, ingress.ConfigPath, http.NoBody))
		if rec.Code != http.StatusOK {
			t.Fatalf("poll = %d %s", rec.Code, rec.Body)
		}
	}
	running := agenttest.Version
	installing := func(key, baseURL string, generation int64) apps.Instance {
		t.Helper()
		inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
			Provider: "testmanaged", Key: key, BaseURL: "https://" + key + ".acme.test",
		})
		if err != nil {
			t.Fatal(err)
		}
		// The agent applied the generation: the instance has its route.
		h.exec(`UPDATE app_instances SET package_id = 'agenttest', package_version = $2, base_url = $3,
			desired_generation = $4, observed_generation = $4, state = 'installing' WHERE id = $1`,
			inst.ID, running, baseURL, generation)
		return h.reload(inst.ID)
	}
	setup := func(inst apps.Instance, attempt int) error {
		return worker.RunSetup(h.ctx, apps.SetupArgs{InstanceID: inst.ID}, attempt, 8)
	}

	// Never served: Setup succeeds, but the attempt fails after the bounded
	// wait and the instance stays installing.
	inst := installing("git", "https://git.acme.test", 1)
	start := time.Now()
	if err := setup(inst, 1); err == nil {
		t.Fatal("setup activated an instance whose route Traefik was never served")
	}
	if d := time.Since(start); d < wait {
		t.Fatalf("the attempt gave up after %v, before the wait %v", d, wait)
	}
	if got := h.reload(inst.ID); got.State != apps.StateInstalling || h.managed.Setups(inst.ID) != 1 {
		t.Fatalf("after an unpublished attempt: %s, %d setups", got.State, h.managed.Setups(inst.ID))
	}

	// Served while the next attempt waits: it activates the instance only
	// after the poll plus the settle time.
	activated := func(inst apps.Instance, attempt int) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- setup(inst, attempt) }()
		time.Sleep(wait / 4)
		if got := h.reload(inst.ID); got.State != apps.StateInstalling {
			t.Fatalf("%s before its route was served: %s", inst.Key, got.State)
		}
		served := time.Now()
		poll()
		if err := <-done; err != nil {
			t.Fatalf("setup after the poll: %v", err)
		}
		if d := time.Since(served); d < settle {
			t.Fatalf("activated %v after the poll, before the settle time %v", d, settle)
		}
		if got := h.reload(inst.ID); got.State != apps.StateActive {
			t.Fatalf("%s after its route was published: %s %s", inst.Key, got.State, derefS(got.HealthCode))
		}
	}
	activated(inst, 2)

	// Removed: a poll drops the route, and recording what it served fails
	// after the answer went out. The omission is recorded before the answer
	// is written, so the reinstall (same host and upstream) on the other
	// process cannot activate on the old evidence: its route must be
	// served again first.
	lossy, err := ingress.NewHandler(cfg, poller.IngressRoutes,
		func(ctx context.Context, routes []ingress.AppRoute) (func(context.Context), error) {
			_, err := poller.PublishRoutes(ctx, routes)
			return nil, err // the recording after the flush is lost
		}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`UPDATE app_instances SET state = 'removed', desired_generation = 2, observed_generation = 2 WHERE id = $1`, inst.ID)
	rec := httptest.NewRecorder()
	lossy.ServeHTTP(rec, httptest.NewRequestWithContext(h.ctx, http.MethodGet, ingress.ConfigPath, http.NoBody))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "git.acme.test") {
		t.Fatalf("the removal poll = %d %s", rec.Code, rec.Body)
	}
	h.exec(`UPDATE app_instances SET state = 'installing', desired_generation = 3, observed_generation = 3 WHERE id = $1`, inst.ID)
	if err := setup(h.reload(inst.ID), 1); err == nil {
		t.Fatal("the reinstall activated on the evidence of a route Traefik no longer has")
	}
	if got := h.reload(inst.ID); got.State != apps.StateInstalling {
		t.Fatalf("the reinstall without a poll: %s", got.State)
	}
	activated(h.reload(inst.ID), 2)

	// A route Build leaves out (here: the workspace host) is never
	// published, however often Traefik polls; the last attempt fails the
	// instance with setup_failed instead of activating it.
	bad := installing("chat", "https://work.acme.test", 1)
	stop := make(chan struct{})
	polled := make(chan struct{})
	go func() {
		defer close(polled)
		for {
			select {
			case <-stop:
				return
			case <-time.After(50 * time.Millisecond):
				poll()
			}
		}
	}()
	err = setup(bad, 8)
	close(stop)
	<-polled
	if err != nil {
		t.Fatalf("last attempt: %v", err)
	}
	if got := h.reload(bad.ID); got.State != apps.StateFailed || derefS(got.HealthCode) != apps.CodeSetupFailed {
		t.Fatalf("an unpublishable route after the last attempt: %s %s", got.State, derefS(got.HealthCode))
	}
	if got := h.reload(inst.ID); got.State != apps.StateActive {
		t.Fatalf("the other instance: %s", got.State)
	}
}

// TestRouteWaitFitsTheSetupDeadline: the route wait never outlasts the
// setup attempt's deadline (River's job timeout), and the last attempt
// records setup_failed even when a slow Setup left (almost) nothing of it,
// instead of leaving the instance installing for good.
func TestRouteWaitFitsTheSetupDeadline(t *testing.T) {
	pkgs, err := packages.NewRegistry(agenttest.FS)
	if err != nil {
		t.Fatal(err)
	}
	const wait = 30 * time.Second
	h := newHarnessWith(t, apps.Config{Packages: pkgs, AwaitRoutes: true, RouteSettle: time.Millisecond, RouteWait: wait})
	for name, left := range map[string]time.Duration{
		// Setup took most of the attempt: what is left after the reserve
		// for the outcome is shorter than the route wait.
		"slow setup": apps.SetupFinishReserve + 500*time.Millisecond,
		// Setup took (almost) all of it: less than the reserve is left, so
		// the route wait has no budget at all.
		"no time left": time.Second,
	} {
		t.Run(name, func(t *testing.T) {
			key := strings.ReplaceAll(name, " ", "")
			inst, err := h.svc.CreateInstance(h.ctx, audit.System, apps.NewInstance{
				Provider: "testmanaged", Key: key, BaseURL: "https://" + key + ".acme.test",
			})
			if err != nil {
				t.Fatal(err)
			}
			h.exec(`UPDATE app_instances SET package_id = 'agenttest', package_version = $2,
				desired_generation = 1, observed_generation = 1, state = 'installing' WHERE id = $1`, inst.ID, agenttest.Version)
			ctx, cancel := context.WithTimeout(h.ctx, left)
			defer cancel()
			start := time.Now()
			// The route is never served (no Traefik poll).
			if err := h.svc.RunSetup(ctx, apps.SetupArgs{InstanceID: inst.ID}, 8, 8); err != nil {
				t.Fatalf("last attempt: %v", err)
			}
			if d := time.Since(start); d > left {
				t.Fatalf("the attempt took %v, longer than its deadline %v", d, left)
			}
			if got := h.reload(inst.ID); got.State != apps.StateFailed || derefS(got.HealthCode) != apps.CodeSetupFailed {
				t.Fatalf("after the last attempt: %s %s, want failed setup_failed", got.State, derefS(got.HealthCode))
			}
		})
	}
}

// TestRoutesServedRecord: the record of what was served to Traefik. A
// route counts once a serve that contains it was flushed (served); an
// unchanged route keeps the time it was first served, a changed one starts
// again, and a route left out is dropped before the answer is written.
// Recording fails closed: a serve whose drop fails is refused (503), and a
// late recording of an earlier serve never brings back a route a newer
// serve dropped.
func TestRoutesServedRecord(t *testing.T) {
	h := newHarnessWith(t, apps.Config{AwaitRoutes: true})
	other := h.restartCore() // another core process on the same database
	git := ingress.AppRoute{Key: "git", Route: "web", Host: "git.acme.test", Upstream: "http://git-web:3000"}
	chat := ingress.AppRoute{Key: "chat", Route: "web", Host: "chat.acme.test", Maintenance: true}
	since := func(key string) (time.Time, bool) {
		t.Helper()
		var at time.Time
		err := h.pool.QueryRow(h.ctx, `SELECT served_since FROM ingress_served_routes WHERE instance_key = $1`, key).Scan(&at)
		if errors.Is(err, pgx.ErrNoRows) {
			return time.Time{}, false
		} else if err != nil {
			t.Fatal(err)
		}
		return at, true
	}
	begin := func(svc *apps.Service, routes ...ingress.AppRoute) func(context.Context) {
		t.Helper()
		served, err := svc.PublishRoutes(h.ctx, routes)
		if err != nil || served == nil {
			t.Fatalf("publish: %v", err)
		}
		return served
	}
	serve := func(svc *apps.Service, routes ...ingress.AppRoute) { begin(svc, routes...)(h.ctx) }

	// Not recorded before the answer was flushed.
	served := begin(h.svc, git)
	if _, ok := since("git"); ok {
		t.Fatal("a route is recorded before its answer was served")
	}
	served(h.ctx)
	first, ok := since("git")
	if !ok {
		t.Fatal("a served route is not recorded")
	}
	serve(other, git, chat)
	if at, _ := since("git"); !at.Equal(first) {
		t.Fatalf("an unchanged route restarted: %v, want %v", at, first)
	}
	if _, ok := since("chat"); !ok {
		t.Fatal("a maintenance route is not recorded")
	}
	moved := git
	moved.Upstream = "http://git-web:4000"
	serve(h.svc, moved, chat)
	if at, _ := since("git"); !at.After(first) {
		t.Fatalf("a changed route kept its time %v", at)
	}

	// Dropped before the answer is written: a lost recording after the
	// flush cannot leave it behind.
	_ = begin(h.svc, chat)
	if _, ok := since("git"); ok {
		t.Fatal("a route left out is still recorded")
	}

	// A late recording of an earlier serve (another process, still in
	// flight when a newer configuration without the route began) records
	// nothing.
	late := begin(other, git, chat)
	serve(h.svc, chat)
	late(h.ctx)
	if _, ok := since("git"); ok {
		t.Fatal("a late recording brought back a route a newer serve dropped")
	}

	// A serve whose drop cannot be recorded is refused: the record keeps
	// describing the configuration Traefik still has.
	tx, err := h.pool.Begin(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(h.ctx, `LOCK TABLE ingress_served_routes IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(h.ctx, 300*time.Millisecond)
	_, err = h.svc.PublishRoutes(ctx, nil)
	cancel()
	_ = tx.Rollback(h.ctx)
	if err == nil {
		t.Fatal("a serve whose drop failed was accepted")
	}
	if _, ok := since("chat"); !ok {
		t.Fatal("a refused serve changed the record")
	}

	serve(h.svc)
	if _, ok := since("chat"); ok {
		t.Fatal("a configuration without app routes kept a route")
	}

	// A deployment without Traefik records nothing.
	if served, err := h.restartCore(func(c *apps.Config) { c.AwaitRoutes = false }).PublishRoutes(h.ctx, []ingress.AppRoute{git}); err != nil || served != nil {
		t.Fatalf("without AwaitRoutes: %v", err)
	}
	if _, ok := since("git"); ok {
		t.Fatal("recorded without AwaitRoutes")
	}
}
