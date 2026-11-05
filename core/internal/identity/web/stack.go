package web

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/agentclient"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/identity/oidcop"
	"github.com/orglane/orglane/core/internal/mail"
	"github.com/orglane/orglane/core/internal/packages"
	"github.com/orglane/orglane/core/internal/peopleimport"
	"github.com/orglane/orglane/core/internal/profile"
	"github.com/orglane/orglane/core/internal/store"
	adapter "github.com/orglane/orglane/providers/provider"
)

// Stack is the identity subsystem wired from the configuration, with the
// profile fields of people (they share the master secret's keyring).
type Stack struct {
	Imports  *peopleimport.Service
	Identity *identity.Service
	Profile  *profile.Service
	Keys     *oidcop.KeyManager
	Provider *oidcop.Provider
	Auth     *Auth
	// Mail sends system email; the job queue delivers what it queues.
	Mail *mail.Service
	// Apps is the provider framework; it observes directory changes to
	// queue provisioning.
	Apps *apps.Service
}

// StackOptions adjust a Stack, mostly for tests.
type StackOptions struct {
	// Hasher replaces the default argon2id hasher (tests use cheaper
	// parameters).
	Hasher *crypto.PasswordHasher
	// Now replaces the clock of signing key rotation.
	Now func() time.Time
	// Adapters replaces the compiled provider adapters (tests register the
	// test provider).
	Adapters []adapter.CapabilityManifest
	// Version is the core's version; the agent client sends mutating
	// requests only to an agent of the same version.
	Version string
	// AppsHTTPClient replaces the HTTP client provider adapters get (tests
	// reach managed services from outside Docker).
	AppsHTTPClient *http.Client
}

// NewStack builds the identity subsystem: the identity service with
// passkeys for the workspace host as relying party ID (ADR 0018), the
// signing keys, the OpenID Provider (issuer: the workspace URL) and the HTTP
// side. It does not touch the database.
func NewStack(pool *pgxpool.Pool, cfg *config.Config, logger *slog.Logger, opts StackOptions) (*Stack, error) {
	keyring, err := crypto.NewKeyring(cfg.Identity.MasterSecret)
	if err != nil {
		return nil, fmt.Errorf("identity.master_secret: %w", err)
	}
	workspaceURL := cfg.WorkspaceURL()
	u, err := url.Parse(workspaceURL)
	if err != nil {
		return nil, err
	}
	rpID := u.Host
	if host, _, err := net.SplitHostPort(u.Host); err == nil {
		rpID = host
	}
	wa, err := webauthn.New(&webauthn.Config{
		RPID:          rpID,
		RPDisplayName: "Orglane",
		RPOrigins:     []string{workspaceURL},
	})
	if err != nil {
		return nil, fmt.Errorf("configure passkeys: %w", err)
	}
	hasher := opts.Hasher
	if hasher == nil {
		hasher = crypto.NewPasswordHasher(crypto.DefaultPasswordParams, runtime.GOMAXPROCS(0))
	}
	mailer := mail.NewService(pool, cfg.Email, cfg.WorkspaceHostname(), keyring, logger)
	localDomain := localServiceDomain(cfg)
	svc := identity.NewService(pool,
		identity.WithLocalServiceDomain(localDomain),
		identity.WithPasswordHasher(hasher),
		identity.WithSignInLimits(identity.SignInLimits{Failures: cfg.Identity.SignInAccountLimit, Lockout: cfg.Identity.SignInLockout}),
		identity.WithWebAuthn(wa),
		identity.WithMailer(mailer),
		identity.WithInvitations(identity.InvitationConfig{WorkspaceURL: workspaceURL, Lifetime: cfg.Identity.InvitationLifetime}),
	)
	keys := oidcop.NewKeyManager(pool, keyring, oidcop.KeyManagerOptions{
		Rotation: cfg.Identity.SigningKeyRotation,
		Grace:    cfg.Identity.SigningKeyGrace,
		Now:      opts.Now,
		Logger:   logger,
	})
	provider, err := oidcop.New(pool, oidcop.Options{
		Issuer:  workspaceURL,
		Keyring: keyring,
		Keys:    keys,
		Lifetimes: oidcop.Lifetimes{
			AccessToken:  cfg.Identity.AccessTokenLifetime,
			IDToken:      cfg.Identity.IDTokenLifetime,
			RefreshToken: cfg.Identity.RefreshTokenLifetime,
		},
		Logger: logger,
	})
	if err != nil {
		return nil, err
	}
	proxies, err := cfg.Server.TrustedProxyPrefixes()
	if err != nil {
		return nil, err
	}
	auth, err := New(Config{
		WorkspaceURL:    workspaceURL,
		IdleTimeout:     cfg.Identity.SessionIdleTimeout,
		AbsoluteTimeout: cfg.Identity.SessionAbsoluteTimeout,
		IPLimit:         cfg.Identity.SignInIPLimit,
		IPWindow:        cfg.Identity.SignInIPWindow,
		TrustedProxies:  proxies,
	}, svc, provider, keyring, logger)
	if err != nil {
		return nil, err
	}
	profiles := profile.NewService(pool, keyring, profile.WithTeams(svc))
	adapters := opts.Adapters
	if adapters == nil {
		adapters = apps.Compiled()
	}
	registry, err := apps.NewRegistry(adapters...)
	if err != nil {
		return nil, err
	}
	issuer, err := url.Parse(provider.Issuer())
	if err != nil {
		return nil, err
	}
	pkgs, err := packages.Embedded()
	if err != nil {
		return nil, err
	}
	appsCfg := apps.Config{
		WorkspaceURL: u, IssuerURL: issuer, Logger: logger,
		Packages: pkgs, OIDC: svc, IngressMode: cfg.Ingress.Mode, ServiceHost: cfg.ServiceHost, SyncInterval: cfg.Agent.SyncInterval,
		HTTPClient: opts.AppsHTTPClient,
	}
	agent, err := agentclient.FromConfig(cfg.Agent, opts.Version)
	if err != nil {
		return nil, fmt.Errorf("agent: %w", err)
	}
	if agent != nil { // a nil *Client must not become a non-nil interface
		appsCfg.Agent = agent
	}
	appsSvc := apps.NewService(pool, registry, keyring, appsCfg)
	svc.SetDirectoryObserver(appsSvc)
	// App clients sign in only people provisioned in their instance.
	auth.SetClientAccess(appsSvc.OIDCAccess)
	return &Stack{Identity: svc, Profile: profiles, Imports: peopleimport.NewService(pool, svc, profiles, keyring, peopleimport.WithLogger(logger)), Keys: keys, Provider: provider, Auth: auth, Mail: mailer, Apps: appsSvc}, nil
}

// Maintain rotates the signing key when due, deletes expired sign-in and
// OIDC state and audits expired invitations. The identity maintenance job
// runs it hourly.
func (s *Stack) Maintain(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := s.Keys.Ensure(ctx); err != nil {
		return err
	}
	if err := s.Provider.Cleanup(ctx); err != nil {
		return err
	}
	q := store.New(pool)
	if _, err := q.DeleteExpiredWebAuthnCeremonies(ctx); err != nil {
		return err
	}
	if _, err := s.Identity.RecordExpiredInvitations(ctx); err != nil {
		return err
	}
	_, err := q.DeleteStaleSignInThrottles(ctx)
	return err
}

// localServiceDomain is the domain whose plain-http redirect URIs OIDC
// clients may use: the base domain, only for a workspace that itself runs
// on plain http in local ingress mode (managed services sign in at
// http://<service>.<base>). An https workspace never gets it, whatever
// the ingress mode says.
func localServiceDomain(cfg *config.Config) string {
	if cfg.Ingress.Mode != config.IngressLocal || cfg.Domain.Scheme != "http" {
		return ""
	}
	return cfg.Domain.Base
}
