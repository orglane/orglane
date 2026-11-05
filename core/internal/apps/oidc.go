package apps

import (
	"context"
	"errors"
	"fmt"

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
