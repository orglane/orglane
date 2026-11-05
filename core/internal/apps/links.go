package apps

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Account linking (docs/specs/providers.md "Account links"). A person proves
// a provider account from their own browser session: the core binds a
// single-use, short-lived state and a PKCE verifier to the person and the
// session, the adapter exchanges the callback's code for the account, and
// the link is keyed by the provider's immutable account ID. Nothing else
// creates a verified link.
//
// Invariants:
//   - One verified link per account (provider, realm, external ID) and one
//     per person per realm (unique indexes); every link change in a realm
//     is serialized by one transaction lock, so the checks below and the
//     insert see the same state.
//   - A state is consumed before the provider is contacted, and only by the
//     person and session it was issued to; a failure never restores it.
//   - Verifying the account another person has linked fails
//     (account_already_linked); verifying a different account replaces the
//     person's own link (the old one is revoked in the same transaction).
//   - Every link change records its audit entry and queues provisioning for
//     the person in every instance of the realm, in the same transaction.

// linkStateTTL bounds how long a person has to complete the provider's
// authorization page.
const linkStateTTL = 10 * time.Minute

// Link evidence (M1 import evidence compared with the verified account).
const (
	EvidenceMatched    = "matched"
	EvidenceMismatched = "mismatched"
	EvidenceNone       = "none"
)

// ErrAccountAlreadyLinked means the account is verified for another person.
var ErrAccountAlreadyLinked = errors.New("apps: the account is already linked to another person")

var linkStatePattern = regexp.MustCompile(`^oll_[A-Za-z0-9_-]{43}$`)

// AccountLinks is what a person sees of their account links.
type AccountLinks struct {
	Verified []store.AccountLink
	// Pending is the person's import evidence for organizations with an
	// active instance; Conflict marks evidence naming an account verified
	// for someone else.
	Pending []store.ListPersonPendingAccountLinksWithConflictRow
	// Linkable are the providers the person can verify an account at.
	Linkable []Instance
}

// MyAccountLinks returns the person's verified links, their import evidence
// for organizations with an active instance, and where they can link.
func (s *Service) MyAccountLinks(ctx context.Context, personID uuid.UUID) (AccountLinks, error) {
	q := store.New(s.pool)
	var out AccountLinks
	var err error
	if out.Verified, err = q.ListPersonAccountLinks(ctx, personID); err != nil {
		return out, fmt.Errorf("apps: list links: %w", err)
	}
	instances, err := s.linkableInstances(ctx, q, "")
	if err != nil {
		return out, err
	}
	orgs := map[int64]bool{}
	seen := map[string]bool{}
	for _, inst := range instances {
		if id, err := strconv.ParseInt(settingString(inst.Settings, "organization_id"), 10, 64); err == nil {
			orgs[id] = true
		}
		if !seen[inst.Provider] {
			seen[inst.Provider] = true
			out.Linkable = append(out.Linkable, inst)
		}
	}
	pending, err := q.ListPersonPendingAccountLinksWithConflict(ctx, personID)
	if err != nil {
		return out, fmt.Errorf("apps: list pending links: %w", err)
	}
	for _, p := range pending {
		if orgs[p.OrganizationID] {
			out.Pending = append(out.Pending, p)
		}
	}
	return out, nil
}

// linkableInstances are the instances, oldest first, whose adapter verifies
// accounts and that are active (or degraded); providerID "" means any.
func (s *Service) linkableInstances(ctx context.Context, q *store.Queries, providerID string) ([]Instance, error) {
	rows, err := q.ListAppInstancesInState(ctx, provisionable)
	if err != nil {
		return nil, fmt.Errorf("apps: list instances: %w", err)
	}
	var out []Instance
	for _, row := range rows {
		if providerID != "" && row.Provider != providerID {
			continue
		}
		inst, err := s.instance(row)
		if err != nil {
			return nil, err
		}
		if inst.Adapter == nil {
			continue
		}
		if _, ok := inst.Adapter.Verifier(); ok {
			out = append(out, inst)
		}
	}
	return out, nil
}

// StartAccountLink begins verification of the person's account at a
// provider: it stores a state and PKCE verifier bound to the person and the
// session and returns the provider URL to send the browser to. Any instance
// of the provider proves the realm's account; the oldest one is used unless
// instanceID names another.
func (s *Service) StartAccountLink(ctx context.Context, personID, sessionID uuid.UUID, providerID string, instanceID *uuid.UUID) (*url.URL, error) {
	if personID == uuid.Nil || sessionID == uuid.Nil {
		return nil, ErrInvalidState
	}
	q := store.New(s.pool)
	instances, err := s.linkableInstances(ctx, q, providerID)
	if err != nil {
		return nil, err
	}
	var inst *Instance
	for i := range instances {
		if instanceID == nil || instances[i].ID == *instanceID {
			inst = &instances[i]
			break
		}
	}
	if inst == nil {
		return nil, fmt.Errorf("%w: no instance verifies %s accounts", ErrNotSupported, providerID)
	}
	verifierImpl, _ := inst.Adapter.Verifier()
	state := crypto.RandomToken("oll_")
	verifier := crypto.RandomToken("")
	sum := sha256.Sum256([]byte(verifier))
	target, err := verifierImpl.BeginVerify(ctx, s.env(*inst), inst.View(), state, base64.RawURLEncoding.EncodeToString(sum[:]))
	if err != nil {
		return nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	err = s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := q.InvalidateAppLinkStates(ctx, store.InvalidateAppLinkStatesParams{PersonID: personID, InstanceID: inst.ID}); err != nil {
			return fmt.Errorf("apps: invalidate link states: %w", err)
		}
		_, err := q.InsertAppOAuthState(ctx, store.InsertAppOAuthStateParams{
			ID: id, Kind: "link", StateHash: crypto.HashToken(state), InstanceID: inst.ID, PersonID: personID, SessionID: sessionID,
			PkceVerifier: s.oauth.Seal([]byte(verifier), id[:]), ExpiresAt: s.cfg.Now().Add(linkStateTTL),
		})
		if err != nil {
			return fmt.Errorf("apps: store link state: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return target, nil
}

// CompleteAccountLink finishes verification: it consumes the state (bound
// to this person and session; single-use even on failure), lets the adapter
// exchange the code for the account and stores the verified link.
func (s *Service) CompleteAccountLink(ctx context.Context, actor audit.Actor, personID, sessionID uuid.UUID, providerID, state, code string) (store.AccountLink, error) {
	if personID == uuid.Nil || sessionID == uuid.Nil || !linkStatePattern.MatchString(state) {
		return store.AccountLink{}, ErrInvalidState
	}
	row, err := store.New(s.pool).ConsumeAppOAuthState(ctx, store.ConsumeAppOAuthStateParams{StateHash: crypto.HashToken(state), Kind: "link", PersonID: personID, SessionID: sessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.AccountLink{}, ErrInvalidState
	} else if err != nil {
		return store.AccountLink{}, fmt.Errorf("apps: consume link state: %w", err)
	}
	inst, err := s.Instance(ctx, row.InstanceID)
	if errors.Is(err, ErrNotFound) {
		return store.AccountLink{}, ErrInvalidState
	} else if err != nil {
		return store.AccountLink{}, err
	}
	if inst.Provider != providerID || inst.Adapter == nil || !inst.Provisionable() {
		return store.AccountLink{}, ErrInvalidState
	}
	verifierImpl, ok := inst.Adapter.Verifier()
	if !ok {
		return store.AccountLink{}, ErrInvalidState
	}
	plain, err := s.oauth.Open(row.PkceVerifier, row.ID[:])
	if err != nil {
		return store.AccountLink{}, fmt.Errorf("apps: open link state: %w", err)
	}
	verifier := string(plain)
	clear(plain)
	acct, err := verifierImpl.CompleteVerify(ctx, s.env(inst), inst.View(), code, verifier)
	if err != nil {
		return store.AccountLink{}, err
	}
	realm := inst.Adapter.Realm(inst.View())
	if acct.Realm != realm || acct.ExternalID == "" || len(acct.ExternalID) > 100 || acct.Login == "" || len(acct.Login) > 100 {
		return store.AccountLink{}, fmt.Errorf("apps: adapter returned an invalid account for realm %s", realm)
	}
	var link store.AccountLink
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if err := q.AcquireXactLock(ctx, lockID("account-links", inst.Provider, realm)); err != nil {
			return fmt.Errorf("apps: lock links: %w", err)
		}
		if cur, err := q.GetVerifiedAccountLinkByAccount(ctx, store.GetVerifiedAccountLinkByAccountParams{Provider: inst.Provider, Realm: realm, ExternalID: acct.ExternalID}); err == nil {
			if cur.PersonID != personID {
				return ErrAccountAlreadyLinked
			}
			// Verified again: only the informational login can have changed.
			if _, err := q.RefreshAccountLinkLogin(ctx, store.RefreshAccountLinkLoginParams{ID: cur.ID, Login: acct.Login}); err != nil {
				return fmt.Errorf("apps: refresh link login: %w", err)
			}
			link, err = q.GetAccountLink(ctx, cur.ID)
			return err
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load link: %w", err)
		}
		if old, err := q.GetVerifiedAccountLink(ctx, store.GetVerifiedAccountLinkParams{PersonID: personID, Provider: inst.Provider, Realm: realm}); err == nil {
			// A different account of the same person replaces the old link;
			// the old account becomes unmanaged (reported, not removed).
			if err := s.revokeLink(ctx, q, actor, old, "replaced"); err != nil {
				return err
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load link: %w", err)
		}
		evidence, err := linkEvidence(ctx, q, inst, personID, acct.ExternalID)
		if err != nil {
			return err
		}
		link, err = q.InsertAccountLink(ctx, store.InsertAccountLinkParams{
			PersonID: personID, Provider: inst.Provider, Realm: realm, ExternalID: acct.ExternalID, Login: acct.Login,
			Method: provider.LinkOAuth, InstanceID: &inst.ID, Evidence: &evidence,
		})
		if err != nil {
			return fmt.Errorf("apps: store link: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AccountLinkVerified,
			Target:   audit.Target{Kind: audit.TargetAccountLink, ID: link.ID.String()},
			Metadata: map[string]any{"provider": inst.Provider, "realm": realm, "instance_id": inst.ID, "person_id": personID, "method": provider.LinkOAuth, "evidence": evidence, "external_id": acct.ExternalID},
		}); err != nil {
			return err
		}
		return s.linkVerified(ctx, tx, q, inst.Provider, realm, personID, uuid.Nil)
	})
	return link, err
}

// linkEvidence compares the verified account with the person's M1 import
// evidence for the instance's organization: matched, mismatched (evidence
// names another account), or none. Evidence never decides; proof wins.
func linkEvidence(ctx context.Context, q *store.Queries, inst Instance, personID uuid.UUID, externalID string) (string, error) {
	org, err := strconv.ParseInt(settingString(inst.Settings, "organization_id"), 10, 64)
	if err != nil {
		return EvidenceNone, nil //nolint:nilerr // no organization: no evidence to compare
	}
	rows, err := q.ListPendingAccountLinksForOrganization(ctx, store.ListPendingAccountLinksForOrganizationParams{Provider: inst.Provider, OrganizationID: org})
	if err != nil {
		return "", fmt.Errorf("apps: list pending links: %w", err)
	}
	evidence := EvidenceNone
	for _, r := range rows {
		if r.PersonID != personID {
			continue
		}
		if strconv.FormatInt(r.ExternalID, 10) == externalID {
			return EvidenceMatched, nil
		}
		evidence = EvidenceMismatched
	}
	return evidence, nil
}

// Who revokes a link (audit metadata "reason").
const (
	RevokedBySelf  = "unlinked"
	RevokedByAdmin = "revoked"
)

// RevokeAccountLink revokes a verified link of the person: by the person
// (unlink) or by an admin with people.manage (a wrong account). Admins can
// revoke, never create. The account becomes unmanaged; provisioning is
// queued for the person in every instance of the realm.
func (s *Service) RevokeAccountLink(ctx context.Context, actor audit.Actor, personID, linkID uuid.UUID, reason string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		link, err := q.GetAccountLink(ctx, linkID)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && (link.PersonID != personID || link.State != "verified")) {
			return fmt.Errorf("%w: account link", ErrNotFound)
		} else if err != nil {
			return fmt.Errorf("apps: load link: %w", err)
		}
		if err := q.AcquireXactLock(ctx, lockID("account-links", link.Provider, link.Realm)); err != nil {
			return fmt.Errorf("apps: lock links: %w", err)
		}
		if err := s.revokeLink(ctx, q, actor, link, reason); err != nil {
			return err
		}
		return s.linkVerified(ctx, tx, q, link.Provider, link.Realm, personID, uuid.Nil)
	})
}

// revokeLink revokes a verified link with its audit entry. The caller holds
// the realm's link lock. Zero rows: it was revoked meanwhile.
func (s *Service) revokeLink(ctx context.Context, q *store.Queries, actor audit.Actor, link store.AccountLink, reason string) error {
	if _, err := q.RevokeAccountLink(ctx, link.ID); errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: account link", ErrNotFound)
	} else if err != nil {
		return fmt.Errorf("apps: revoke link: %w", err)
	}
	_, err := audit.Record(ctx, q, audit.Entry{
		Actor:    actor,
		Action:   audit.AccountLinkRevoked,
		Target:   audit.Target{Kind: audit.TargetAccountLink, ID: link.ID.String()},
		Metadata: map[string]any{"provider": link.Provider, "realm": link.Realm, "person_id": link.PersonID, "reason": reason},
	})
	return err
}
