package apps

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// registerOIDCClient registers the OIDC client of a relying-party instance
// (provider.OIDCRelyingParty) in the transaction that creates the instance,
// and stores its secret as the adapter's SourceIdentity credential: the
// instance, its client and the secret exist together or not at all. The
// client's audit entry is written by identity in the same transaction.
func (s *Service) registerOIDCClient(ctx context.Context, q *store.Queries, actor audit.Actor, inst Instance) error {
	rp, ok := inst.Adapter.RelyingParty()
	if !ok {
		return nil
	}
	if s.cfg.OIDC == nil {
		return fmt.Errorf("%w: OIDC clients cannot be registered here", ErrNotSupported)
	}
	if inst.BaseUrl == nil {
		return invalid("base_url", "is required: the OIDC client's redirect URIs are built from it")
	}
	spec, err := rp.OIDCClient(inst.View())
	if err != nil {
		return fmt.Errorf("apps: OIDC client of %s: %w", inst.Provider, err)
	}
	cred, ok := inst.Adapter.Credential(spec.Secret)
	if !ok || cred.Source != provider.SourceIdentity {
		return fmt.Errorf("apps: OIDC client of %s: secret %q is no %s credential", inst.Provider, spec.Secret, provider.SourceIdentity)
	}
	_, secret, err := s.cfg.OIDC.CreateOIDCClientTx(ctx, q, actor, identity.NewOIDCClient{
		ID:                     provider.OIDCClientID(inst.Key),
		Name:                   inst.DisplayName,
		AuthMethod:             identity.ClientAuthBasic,
		RedirectURIs:           spec.RedirectURIs,
		PostLogoutRedirectURIs: spec.PostLogoutRedirectURIs,
	})
	if errors.Is(err, identity.ErrInvalid) {
		// A redirect URI identity refuses, or a client app-<key> that exists
		// already (registered by an operator).
		return invalid("oidc_client", "%v", err)
	} else if err != nil {
		return fmt.Errorf("apps: register the OIDC client: %w", err)
	}
	_, err = s.storeCredential(ctx, q, actor, inst, spec.Secret, []byte(secret), credentialActive)
	return err
}

// OIDCAccess decides whether a person may sign in to an OIDC client
// (identity/web ClientAccess). Clients of app instances
// (provider.OIDCClientID) sign in only people Orglane provisioned in that
// instance: a verified "provisioned" link the instance established, and
// access at the last provisioning run, while the instance is active or
// degraded. Everyone else is refused (access_denied), so the service never
// learns of a subject it has no account for.
//
// This is what keeps Forgejo from linking accounts: when its callback gets
// a subject no account is bound to while a Forgejo session is open, it
// links that subject to the session's account, whatever ACCOUNT_LINKING
// says (Forgejo 15, routers/web/auth/oauth.go). With this check Forgejo
// only ever gets subjects whose own account Orglane created, and those
// sign in to that account.
//
// Other clients (registered by an operator) are not app clients: allowed.
func (s *Service) OIDCAccess(ctx context.Context, clientID string, personID uuid.UUID) (bool, error) {
	key, ok := strings.CutPrefix(clientID, provider.OIDCClientID(""))
	if !ok {
		return true, nil
	}
	q := store.New(s.pool)
	rows, err := q.ListAppInstances(ctx)
	if err != nil {
		return false, fmt.Errorf("apps: list instances: %w", err)
	}
	var inst *Instance
	for _, row := range rows {
		if row.Key != key || (row.State == StateRemoved && !row.DataRetained) {
			continue
		}
		i, err := s.instance(row)
		if err != nil {
			return false, err
		}
		inst = &i
	}
	if inst == nil || inst.Adapter == nil {
		return true, nil // no app instance owns this client
	}
	if _, rp := inst.Adapter.RelyingParty(); !rp {
		return true, nil
	}
	if !inst.Provisionable() {
		return false, nil
	}
	link, err := q.GetVerifiedAccountLink(ctx, store.GetVerifiedAccountLinkParams{PersonID: personID, Provider: inst.Provider, Realm: inst.Adapter.Realm(inst.View())})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("apps: load link: %w", err)
	}
	if link.Method != provider.LinkProvisioned || link.InstanceID == nil || *link.InstanceID != inst.ID {
		return false, nil
	}
	result, err := q.GetProvisioningResult(ctx, store.GetProvisioningResultParams{InstanceID: inst.ID, PersonID: personID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("apps: load result: %w", err)
	}
	return result.Access, nil
}
