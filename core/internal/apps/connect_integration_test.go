//go:build integration

package apps_test

import (
	"context"
	"errors"
	"hash/fnv"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

func githubPoolService(t *testing.T, h *harness, size int32) (*apps.Service, *pgxpool.Pool) {
	t.Helper()
	cfg := h.pool.Config()
	cfg.MaxConns = size
	cfg.MinConns = 0
	cfg.ConnConfig.RuntimeParams["application_name"] = "connect_pool_" + strconv.Itoa(int(size))
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	reg, err := apps.NewRegistry(githubprovider.New())
	if err != nil {
		t.Fatal(err)
	}
	ring, err := crypto.NewKeyring(masterSecret)
	if err != nil {
		t.Fatal(err)
	}
	return apps.NewService(pool, reg, ring, apps.Config{}), pool
}

func TestConnectMinimumPool(t *testing.T) {
	h := newHarness(t)
	svc, pool := githubPoolService(t, h, 1)
	person := h.person("connect-minimum")
	session := uuid.UUID(h.session(person))
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	reply, err := svc.BeginGitHubConnect(ctx, audit.System, person.ID, session, "orglane-test", "github-minimum", nil, nil)
	if err != nil {
		t.Fatalf("prepare must complete with max_conns=1: %v", err)
	}
	// Wrong-step callbacks still consume a valid bound state without contacting GitHub.
	state := reply.Step.URL.Query().Get("state")
	_, err = svc.CompleteGitHubConnect(ctx, audit.System, person.ID, session, state, url.Values{"installation_id": {"123"}})
	if !errors.Is(err, apps.ErrInvalidState) {
		t.Fatalf("callback consumption reacquired the pool: %v", err)
	}
	var consumed bool
	if err = pool.QueryRow(ctx, "SELECT consumed_at IS NOT NULL FROM app_oauth_states WHERE state_hash=$1", crypto.HashToken(state)).Scan(&consumed); err != nil || !consumed {
		t.Fatalf("state was not consumed: %v", err)
	}
	// The install leg reads its sealed key through the lock connection as well.
	inst, err := svc.CreateInstance(ctx, audit.System, apps.NewInstance{Provider: "github", Key: "github-install", Settings: map[string]any{"organization": "orglane-test", "organization_id": "123", "app_id": "456", "app_slug": "orglane-ci"}})
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.StoreCredential(ctx, audit.System, inst.ID, "app_private_key", []byte("invalid-local-fixture")); err != nil {
		t.Fatal(err)
	}
	reply, err = svc.BeginGitHubConnect(ctx, audit.System, person.ID, session, "orglane-test", inst.Key, &inst.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = svc.CompleteGitHubConnect(ctx, audit.System, person.ID, session, reply.Step.URL.Query().Get("state"), url.Values{"installation_id": {"789"}})
	var pe *provider.Error
	if !errors.As(err, &pe) || pe.Code != "invalid_private_key" {
		t.Fatalf("credential read must finish before local key rejection, without network: %v", err)
	}
}

func TestConnectConcurrentDefaultPool(t *testing.T) {
	h := newHarness(t)
	svc, pool := githubPoolService(t, h, 10)
	person := h.person("connect-concurrent")
	session := uuid.UUID(h.session(person))
	inst, err := svc.CreateInstance(t.Context(), audit.System, apps.NewInstance{Provider: "github", Key: "github-concurrent", Settings: map[string]any{"organization": "orglane-test"}})
	if err != nil {
		t.Fatal(err)
	}
	f := fnv.New64a()
	_, _ = f.Write([]byte("orglane.apps"))
	for _, p := range []string{"connect", inst.ID.String()} {
		_, _ = f.Write([]byte{0})
		_, _ = f.Write([]byte(p))
	}
	lock := int64(f.Sum64()) //nolint:gosec // Advisory lock uses the complete signed 64-bit hash bit pattern, matching lockID.
	blocker, err := h.pool.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Release()
	if _, err = blocker.Exec(t.Context(), "SELECT pg_advisory_lock($1)", lock); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 20)
	for range 20 {
		go func() {
			_, e := svc.BeginGitHubConnect(ctx, audit.System, person.ID, session, "orglane-test", inst.Key, &inst.ID, nil)
			done <- e
		}()
	}
	deadline := time.Now().Add(2 * time.Second)
	waiting := 0
	for time.Now().Before(deadline) {
		if err = h.pool.QueryRow(t.Context(), "SELECT count(*) FROM pg_stat_activity WHERE application_name='connect_pool_10' AND wait_event='advisory'").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == 10 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err = blocker.Exec(t.Context(), "SELECT pg_advisory_unlock($1)", lock); err != nil {
		t.Fatal(err)
	}
	if waiting != 10 {
		t.Errorf("regression setup did not occupy all default-pool lock slots: %d", waiting)
	}
	for range 20 {
		if err = <-done; err != nil {
			t.Errorf("serialized resume must complete without acquiring another pool slot: %v", err)
		}
	}
	var active int
	if err = pool.QueryRow(t.Context(), "SELECT count(*) FROM app_oauth_states WHERE instance_id=$1 AND consumed_at IS NULL", inst.ID).Scan(&active); err != nil || active != 1 {
		t.Fatalf("concurrent resumes must leave exactly one usable state: %d %v", active, err)
	}
	if err = pool.QueryRow(t.Context(), "SELECT count(*) FROM app_oauth_states WHERE instance_id=$1", inst.ID).Scan(&active); err != nil || active != 20 {
		t.Fatalf("expected each resume to persist its new state: %d %v", active, err)
	}
}
