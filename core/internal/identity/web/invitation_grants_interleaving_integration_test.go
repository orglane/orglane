//go:build integration

package web_test

import (
	"bytes"
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

// The promotion remains uncommitted while the API precheck observes the old
// member grant. The request must wait on the domain grant lock and revalidate
// after the promotion commits, rather than capturing an elevated capability.
func TestInvitationManagementRevalidatesConcurrentPromotion(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			e := newEnv(t)
			owner := e.browser()
			e.setupOwner(owner)
			adminPerson := e.createMember("grantadmin")
			if err := e.stack.Identity.AssignRole(t.Context(), audit.System, adminPerson.ID, authz.RoleAdmin); err != nil {
				t.Fatal(err)
			}
			admin := e.browser()
			admin.signIn(adminPerson.WorkEmail)
			issued := owner.invite("Promoted Invite", "promoted@home.test", "promoted")
			before, err := store.New(e.pool).GetInvitation(t.Context(), issued.Invitation.ID)
			if err != nil {
				t.Fatal(err)
			}
			promotion, err := e.pool.Begin(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = promotion.Rollback(context.Background()) }()
			q := store.New(promotion)
			if err = identity.LockGrantsTx(t.Context(), q); err != nil {
				t.Fatal(err)
			}
			if _, err = q.GetPersonForUpdate(t.Context(), before.PersonID); err != nil {
				t.Fatal(err)
			}
			elevated, err := q.GetRoleByKey(t.Context(), string(authz.RoleOwner))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = q.AssignPersonRole(t.Context(), store.AssignPersonRoleParams{PersonID: before.PersonID, RoleID: elevated.ID}); err != nil {
				t.Fatal(err)
			}
			if _, err = audit.Record(t.Context(), q, audit.Entry{Actor: audit.System, Action: audit.PersonRoleAssigned, Target: audit.Target{Kind: audit.TargetPerson, ID: before.PersonID.String()}, Metadata: map[string]any{"role_id": elevated.ID}}); err != nil {
				t.Fatal(err)
			}
			path := "/api/v1/invitations/" + before.ID.String()
			if method == http.MethodPost {
				path += "/resend"
			}
			done := make(chan response, 1)
			go func() { done <- admin.req(method, path, nil) }()
			deadline := time.Now().Add(5 * time.Second)
			waited := false
			for time.Now().Before(deadline) {
				var n int
				if err = e.pool.QueryRow(t.Context(), `SELECT count(*) FROM pg_stat_activity WHERE $1::bigint = ANY(pg_blocking_pids(pid)::bigint[]) AND wait_event_type = 'Lock'`, int64(promotion.Conn().PgConn().PID())).Scan(&n); err != nil {
					t.Fatal(err)
				}
				if n > 0 {
					waited = true
					break
				}
				select {
				case r := <-done:
					t.Fatalf("request did not wait for promotion: %d", r.StatusCode)
				default:
				}
				time.Sleep(10 * time.Millisecond)
			}
			if !waited {
				t.Fatal("request never reached locked grant authorization")
			}
			if err = promotion.Commit(t.Context()); err != nil {
				t.Fatal(err)
			}
			select {
			case r := <-done:
				expectStatus(t, r, http.StatusForbidden)
				if bytes.Contains(r.body, []byte("oli_")) {
					t.Fatal("elevated capability disclosed")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("request remained blocked after promotion")
			}
			after, err := store.New(e.pool).GetInvitation(t.Context(), before.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before.TokenHash, after.TokenHash) || after.RevokedAt != nil || !after.IssuedAt.Equal(before.IssuedAt) {
				t.Fatal("denied request mutated invitation")
			}
			var changed int
			if err = e.pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_entries WHERE target_id=$1 AND action IN ('invitation.resent','invitation.revoked')`, before.ID.String()).Scan(&changed); err != nil || changed != 0 {
				t.Fatalf("unauthorized audit effects: %d, %v", changed, err)
			}
		})
	}
}
