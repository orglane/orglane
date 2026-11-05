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
		AppInstance:            true,
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
// (identity/web ClientAccess, oidcop ClientAccess). Client IDs that start
// with provider.OIDCClientID("") ("app-") are reserved for app instances
// (identity refuses them for other clients), so such a client is always an
// app client, and it signs in only people Orglane provisioned in its
// instance: a verified "provisioned" link the instance established and
// access at the last provisioning run, while the instance is active or
// degraded. Every case that cannot be decided denies: no instance for the
// key (purged), a removed instance, an adapter this build does not have, an
// adapter that is no relying party, and errors (returned, so the caller
// fails closed). Clients without the prefix are an operator's: allowed.
//
// This keeps Forgejo from linking accounts: when its callback gets a
// subject no account is bound to while a Forgejo session is open, it links
// that subject to the session's account, whatever ACCOUNT_LINKING says
// (Forgejo 15, routers/web/auth/oauth.go). With this check Forgejo only
// ever gets subjects whose own account Orglane created.
//
// OIDCAccess reads without a transaction (the early check when a sign-in
// completes); OIDCAccessTx runs in the transaction that issues a code or
// tokens and holds share locks on the rows it decided by, so a withdrawal
// commits before the issuance (and is seen) or after it, never in between.
func (s *Service) OIDCAccess(ctx context.Context, clientID string, personID uuid.UUID) (bool, error) {
	return s.OIDCAccessTx(ctx, store.New(s.pool), clientID, personID)
}

// OIDCAccessTx is OIDCAccess in the caller's transaction (q), locking what
// it reads.
func (s *Service) OIDCAccessTx(ctx context.Context, q *store.Queries, clientID string, personID uuid.UUID) (bool, error) {
	key, ok := strings.CutPrefix(clientID, provider.OIDCClientID(""))
	if !ok {
		return true, nil // an operator's client
	}
	row, err := q.GetAppInstanceByKeyForShare(ctx, key)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil // purged, or never an instance
	} else if err != nil {
		return false, fmt.Errorf("apps: load instance: %w", err)
	}
	inst, err := s.instance(row)
	if err != nil {
		return false, err
	}
	if inst.Adapter == nil || !inst.Provisionable() {
		return false, nil
	}
	if _, rp := inst.Adapter.RelyingParty(); !rp {
		return false, nil
	}
	link, err := q.GetVerifiedAccountLinkForShare(ctx, store.GetVerifiedAccountLinkForShareParams{PersonID: personID, Provider: inst.Provider, Realm: inst.Adapter.Realm(inst.View())})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("apps: load link: %w", err)
	}
	if link.Method != provider.LinkProvisioned || link.InstanceID == nil || *link.InstanceID != inst.ID {
		return false, nil
	}
	result, err := q.GetProvisioningResultForShare(ctx, store.GetProvisioningResultForShareParams{InstanceID: inst.ID, PersonID: personID})
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("apps: load result: %w", err)
	}
	return result.Access, nil
}
