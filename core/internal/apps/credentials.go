package apps

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// Credential states (docs/specs/providers.md "Credentials").
const (
	credentialPending   = "pending"
	credentialUncertain = "uncertain"
	credentialActive    = "active"
	credentialRetiring  = "retiring"
	credentialDiscarded = "discarded"
)

// defaultOverlap is how long a replaced version stays valid when the spec
// declares no overlap.
const defaultOverlap = time.Hour

// stuckAfter is when an uncertain candidate is reported to admins.
const stuckAfter = 24 * time.Hour

// credentialAD binds a sealed credential to its row.
func credentialAD(instanceID uuid.UUID, name string, version int32) []byte {
	return []byte("app-credential\x00" + instanceID.String() + "\x00" + name + "\x00" + strconv.Itoa(int(version)))
}

// storeCredential seals and stores a new version of a credential.
func (s *Service) storeCredential(ctx context.Context, q *store.Queries, actor audit.Actor, inst Instance, name string, value []byte, state string) (store.AppCredential, error) {
	version, err := q.NextAppCredentialVersion(ctx, store.NextAppCredentialVersionParams{InstanceID: inst.ID, Name: name})
	if err != nil {
		return store.AppCredential{}, fmt.Errorf("apps: next credential version: %w", err)
	}
	row, err := q.InsertAppCredentialVersion(ctx, store.InsertAppCredentialVersionParams{
		InstanceID: inst.ID,
		Name:       name,
		Version:    version,
		Ciphertext: s.creds.Seal(value, credentialAD(inst.ID, name, version)),
		State:      state,
	})
	if err != nil {
		return store.AppCredential{}, fmt.Errorf("apps: store credential: %w", err)
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    actor,
		Action:   audit.AppCredentialCreated,
		Target:   instanceTarget(inst.ID),
		Metadata: map[string]any{"provider": inst.Provider, "name": name, "version": version, "state": state},
	})
	return row, err
}

// StoreCredential stores a credential obtained from the provider or an
// admin (Connect, Setup, uploads) as the new active version; the previous
// active version retires after the spec's overlap.
func (s *Service) StoreCredential(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string, value []byte) error {
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		inst, err := s.loadInstance(ctx, q, instanceID, true)
		if err != nil {
			return err
		}
		if inst.Adapter == nil {
			return ErrProviderUnavailable
		}
		spec, ok := inst.Adapter.Credential(name)
		if !ok {
			return fmt.Errorf("%w: credential", ErrNotFound)
		}
		if err := q.RetireActiveAppCredential(ctx, store.RetireActiveAppCredentialParams{InstanceID: inst.ID, Name: name, RetireAt: ptr(s.cfg.Now().Add(overlap(spec)))}); err != nil {
			return fmt.Errorf("apps: retire credential: %w", err)
		}
		_, err = s.storeCredential(ctx, q, actor, inst, name, value, credentialActive)
		return err
	})
}

func (s *Service) openCredential(row store.AppCredential) ([]byte, error) {
	plain, err := s.creds.Open(row.Ciphertext, credentialAD(row.InstanceID, row.Name, row.Version))
	if err != nil {
		return nil, fmt.Errorf("apps: open credential %s v%d: %w", row.Name, row.Version, err)
	}
	return plain, nil
}

// Credential is a credential version without its value.
type Credential = store.ListAppCredentialsRow

// Credentials returns the credential versions of an instance, without
// values, discarded ones excluded.
func (s *Service) Credentials(ctx context.Context, instanceID uuid.UUID) ([]Credential, error) {
	rows, err := store.New(s.pool).ListAppCredentials(ctx, instanceID)
	if err != nil {
		return nil, fmt.Errorf("apps: list credentials: %w", err)
	}
	return rows, nil
}

// RotationStuck reports whether a candidate of the instance has been
// uncertain for longer than a day.
func RotationStuck(creds []Credential, now time.Time) bool {
	for _, c := range creds {
		if c.State == credentialUncertain && now.Sub(c.CreatedAt) > stuckAfter {
			return true
		}
	}
	return false
}

// RotateCredential starts a staged rotation: the new version is stored as
// pending and handed to the provider by the rotate_credential job. The
// active version stays in use until the provider acknowledges the new one.
func (s *Service) RotateCredential(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string) (int32, error) {
	var version int32
	err := s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, spec, err := s.rotatable(ctx, q, instanceID, name)
		if err != nil {
			return err
		}
		if _, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: instanceID, Name: name}); err == nil {
			return ErrRotationInProgress
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("apps: load candidate: %w", err)
		}
		var value []byte // minted by the provider
		if spec.Source == provider.SourceGenerated {
			value = []byte(crypto.RandomToken(""))
		}
		row, err := s.storeCredential(ctx, q, actor, inst, name, value, credentialPending)
		if err != nil {
			return mapCandidateConflict(err)
		}
		version = row.Version
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppCredentialRotationStarted,
			Target:   instanceTarget(instanceID),
			Metadata: map[string]any{"provider": inst.Provider, "name": name, "version": version},
		}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, RotateCredentialArgs{InstanceID: instanceID, Name: name, Version: version}, nil)
	})
	return version, err
}

// AbandonRotation gives up an uncertain (or pending) candidate. The job
// first re-applies the active value to the provider until it is
// acknowledged, and only then discards the candidate.
func (s *Service) AbandonRotation(ctx context.Context, actor audit.Actor, instanceID uuid.UUID, name string) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		inst, _, err := s.rotatable(ctx, q, instanceID, name)
		if err != nil {
			return err
		}
		cand, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: instanceID, Name: name})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoRotation
		} else if err != nil {
			return fmt.Errorf("apps: load candidate: %w", err)
		}
		if _, err := audit.Record(ctx, q, audit.Entry{
			Actor:    actor,
			Action:   audit.AppCredentialRotationAbandoned,
			Target:   instanceTarget(instanceID),
			Metadata: map[string]any{"provider": inst.Provider, "name": name, "version": cand.Version, "stage": "requested"},
		}); err != nil {
			return err
		}
		return s.enqueue(ctx, tx, RotateCredentialArgs{InstanceID: instanceID, Name: name, Version: cand.Version, Abandon: true}, nil)
	})
}

func (s *Service) rotatable(ctx context.Context, q *store.Queries, instanceID uuid.UUID, name string) (Instance, provider.CredentialSpec, error) {
	inst, err := s.loadInstance(ctx, q, instanceID, true)
	if err != nil {
		return Instance{}, provider.CredentialSpec{}, err
	}
	if inst.State == StateRemoved || inst.State == StateRemoving {
		return Instance{}, provider.CredentialSpec{}, ErrInvalidState
	}
	if inst.Adapter == nil {
		return Instance{}, provider.CredentialSpec{}, ErrProviderUnavailable
	}
	spec, ok := inst.Adapter.Credential(name)
	if !ok {
		return Instance{}, provider.CredentialSpec{}, fmt.Errorf("%w: credential", ErrNotFound)
	}
	if !spec.Rotatable || spec.Source == provider.SourceUpload {
		return Instance{}, provider.CredentialSpec{}, fmt.Errorf("%w: the credential is not rotated by Orglane", ErrNotSupported)
	}
	if spec.Agent {
		// The agent consumes it through a new desired generation (#59, #60).
		return Instance{}, provider.CredentialSpec{}, fmt.Errorf("%w: agent credentials rotate with the managed service", ErrNotSupported)
	}
	if _, ok := inst.Adapter.Rotator(); !ok {
		return Instance{}, provider.CredentialSpec{}, ErrNotSupported
	}
	return inst, spec, nil
}

func mapCandidateConflict(err error) error {
	if isUnique(err, "app_credentials_pending_key") {
		return ErrRotationInProgress
	}
	return err
}

// errRotationUnknown makes the job retry: the provider may or may not have
// committed the change.
var errRotationUnknown = errors.New("apps: the provider's answer to a credential change is unknown")

// RunRotateCredential works one attempt of a rotate_credential job.
func (s *Service) RunRotateCredential(ctx context.Context, args RotateCredentialArgs) error {
	q := store.New(s.pool)
	inst, err := s.loadInstance(ctx, q, args.InstanceID, false)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if inst.Adapter == nil {
		return river.JobCancel(ErrProviderUnavailable)
	}
	rotator, ok := inst.Adapter.Rotator()
	spec, okSpec := inst.Adapter.Credential(args.Name)
	if !ok || !okSpec {
		return river.JobCancel(ErrNotSupported)
	}
	cand, err := q.GetAppCredential(ctx, store.GetAppCredentialParams{InstanceID: args.InstanceID, Name: args.Name, Version: args.Version})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // cleaned up
	} else if err != nil {
		return fmt.Errorf("apps: load candidate: %w", err)
	}
	if cand.State != credentialPending && cand.State != credentialUncertain {
		return nil // resolved by another path (proof by use, an earlier attempt)
	}
	env := s.env(inst)
	if args.Abandon {
		return s.abandon(ctx, inst, rotator, env, cand)
	}
	var value []byte
	if spec.Source == provider.SourceGenerated {
		if value, err = s.openCredential(cand); err != nil {
			return river.JobCancel(err)
		}
	}
	// The candidate is uncertain from the moment it is handed over: a worker
	// dying mid-call must not leave it looking untried.
	if cand.State == credentialPending {
		if err := q.SetAppCredentialState(ctx, store.SetAppCredentialStateParams{InstanceID: cand.InstanceID, Name: cand.Name, Version: cand.Version, State: credentialUncertain}); err != nil {
			return fmt.Errorf("apps: mark candidate uncertain: %w", err)
		}
	}
	res, err := rotator.ApplyCredential(ctx, env, inst.View(), args.Name, int(args.Version), value)
	if err != nil {
		res.Outcome = provider.RotationUnknown
	}
	switch res.Outcome {
	case provider.RotationAcknowledged:
		return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
			if res.Value != nil {
				if err := q.SetAppCredentialCiphertext(ctx, store.SetAppCredentialCiphertextParams{
					InstanceID: cand.InstanceID, Name: cand.Name, Version: cand.Version,
					Ciphertext: s.creds.Seal(res.Value, credentialAD(cand.InstanceID, cand.Name, cand.Version)),
				}); err != nil {
					return fmt.Errorf("apps: store minted credential: %w", err)
				}
			}
			return s.acknowledge(ctx, q, inst, spec, cand, "applied")
		})
	case provider.RotationRejected:
		return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
			if err := q.SetAppCredentialState(ctx, store.SetAppCredentialStateParams{InstanceID: cand.InstanceID, Name: cand.Name, Version: cand.Version, State: credentialDiscarded}); err != nil {
				return fmt.Errorf("apps: discard candidate: %w", err)
			}
			_, err := audit.Record(ctx, q, audit.Entry{
				Actor:    audit.System,
				Action:   audit.AppCredentialRotationFailed,
				Target:   instanceTarget(inst.ID),
				Metadata: map[string]any{"provider": inst.Provider, "name": cand.Name, "version": cand.Version},
			})
			return err
		})
	default:
		if err := s.reportStuck(ctx, inst, cand); err != nil {
			return err
		}
		if err != nil {
			code, _, _ := provider.Classify(err)
			return fmt.Errorf("%w (%s)", errRotationUnknown, code)
		}
		return errRotationUnknown
	}
}

// acknowledge makes the candidate the active version; the old active
// version retires after the overlap.
func (s *Service) acknowledge(ctx context.Context, q *store.Queries, inst Instance, spec provider.CredentialSpec, cand store.AppCredential, how string) error {
	cur, err := q.GetAppCredentialForUpdate(ctx, store.GetAppCredentialForUpdateParams{InstanceID: cand.InstanceID, Name: cand.Name, Version: cand.Version})
	if err != nil {
		return fmt.Errorf("apps: lock candidate: %w", err)
	}
	if cur.State != credentialPending && cur.State != credentialUncertain {
		return nil
	}
	if err := q.RetireActiveAppCredential(ctx, store.RetireActiveAppCredentialParams{InstanceID: cand.InstanceID, Name: cand.Name, RetireAt: ptr(s.cfg.Now().Add(overlap(spec)))}); err != nil {
		return fmt.Errorf("apps: retire credential: %w", err)
	}
	if err := q.ActivateAppCredential(ctx, store.ActivateAppCredentialParams{InstanceID: cand.InstanceID, Name: cand.Name, Version: cand.Version}); err != nil {
		return fmt.Errorf("apps: activate credential: %w", err)
	}
	_, err = audit.Record(ctx, q, audit.Entry{
		Actor:    audit.System,
		Action:   audit.AppCredentialRotated,
		Target:   instanceTarget(inst.ID),
		Metadata: map[string]any{"provider": inst.Provider, "name": cand.Name, "version": cand.Version, "proof": how},
	})
	return err
}

func (s *Service) abandon(ctx context.Context, inst Instance, rotator provider.CredentialRotator, env provider.Env, cand store.AppCredential) error {
	q := store.New(s.pool)
	active, err := q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: cand.InstanceID, Name: cand.Name})
	if err != nil {
		return fmt.Errorf("apps: load active credential: %w", err)
	}
	value, err := s.openCredential(active)
	if err != nil {
		return river.JobCancel(err)
	}
	res, err := rotator.ApplyCredential(ctx, env, inst.View(), cand.Name, int(active.Version), value)
	if err != nil || res.Outcome != provider.RotationAcknowledged {
		return errRotationUnknown // keep re-applying the active value
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		if err := q.SetAppCredentialState(ctx, store.SetAppCredentialStateParams{InstanceID: cand.InstanceID, Name: cand.Name, Version: cand.Version, State: credentialDiscarded}); err != nil {
			return fmt.Errorf("apps: discard candidate: %w", err)
		}
		_, err := audit.Record(ctx, q, audit.Entry{
			Actor:    audit.System,
			Action:   audit.AppCredentialRotationAbandoned,
			Target:   instanceTarget(inst.ID),
			Metadata: map[string]any{"provider": inst.Provider, "name": cand.Name, "version": cand.Version, "stage": "discarded"},
		})
		return err
	})
}

// reportStuck records rotation_stuck once for a candidate uncertain for a
// day. Nothing is retired or discarded automatically.
func (s *Service) reportStuck(ctx context.Context, inst Instance, cand store.AppCredential) error {
	if s.cfg.Now().Sub(cand.CreatedAt) <= stuckAfter {
		return nil
	}
	return s.inTx(ctx, func(_ pgx.Tx, q *store.Queries) error {
		var exists bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit_entries WHERE action = $1 AND target_kind = $2 AND target_id = $3
			AND metadata->>'name' = $4 AND metadata->>'version' = $5)`,
			string(audit.AppCredentialRotationStuck), string(audit.TargetAppInstance), inst.ID.String(), cand.Name, strconv.Itoa(int(cand.Version))).Scan(&exists); err != nil {
			return fmt.Errorf("apps: check stuck report: %w", err)
		}
		if exists {
			return nil
		}
		_, err := audit.Record(ctx, q, audit.Entry{
			Actor:    audit.System,
			Action:   audit.AppCredentialRotationStuck,
			Target:   instanceTarget(inst.ID),
			Metadata: map[string]any{"provider": inst.Provider, "name": cand.Name, "version": strconv.Itoa(int(cand.Version))},
		})
		return err
	})
}

// proveByUse acknowledges an uncertain webhook secret when a verified
// delivery verifies only with it: the provider signs with the new secret.
// A delivery that verifies with the active secret proves nothing.
func (s *Service) proveByUse(ctx context.Context, q *store.Queries, inst Instance, src provider.EventSource, h http.Header, body []byte) error {
	const name = "webhook_secret"
	spec, ok := inst.Adapter.Credential(name)
	if !ok {
		return nil
	}
	cand, err := q.GetAppCredentialCandidate(ctx, store.GetAppCredentialCandidateParams{InstanceID: inst.ID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("apps: load candidate: %w", err)
	}
	candValue, err := s.openCredential(cand)
	if err != nil {
		return err
	}
	if verr := src.Verify(ctx, fixedSecrets{s.env(inst), name, [][]byte{candValue}}, inst.View(), h, body); verr != nil {
		return nil //nolint:nilerr // signed with another secret: no proof either way
	}
	if active, err := q.GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: inst.ID, Name: name}); err == nil {
		activeValue, err := s.openCredential(active)
		if err != nil {
			return err
		}
		if src.Verify(ctx, fixedSecrets{s.env(inst), name, [][]byte{activeValue}}, inst.View(), h, body) == nil {
			return nil // signed with both: proves nothing
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("apps: load active credential: %w", err)
	}
	return s.acknowledge(ctx, q, inst, spec, cand, "use")
}

func overlap(spec provider.CredentialSpec) time.Duration {
	if spec.Overlap > 0 {
		return spec.Overlap
	}
	return defaultOverlap
}

func ptr[T any](v T) *T { return &v }

// env is the Env handed to adapters for an instance.
type env struct {
	s    *Service
	inst Instance
}

func (s *Service) env(inst Instance) provider.Env { return &env{s: s, inst: inst} }

func (e *env) Credential(ctx context.Context, name string) ([]byte, error) {
	row, err := store.New(e.s.pool).GetActiveAppCredential(ctx, store.GetActiveAppCredentialParams{InstanceID: e.inst.ID, Name: name})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, provider.ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("apps: load credential: %w", err)
	}
	return e.s.openCredential(row)
}

func (e *env) Credentials(ctx context.Context, name string) ([][]byte, error) {
	rows, err := store.New(e.s.pool).ListUsableAppCredentials(ctx, store.ListUsableAppCredentialsParams{InstanceID: e.inst.ID, Name: name})
	if err != nil {
		return nil, fmt.Errorf("apps: load credentials: %w", err)
	}
	out := make([][]byte, 0, len(rows))
	for _, row := range rows {
		v, err := e.s.openCredential(row)
		if err != nil {
			return nil, err
		}
		if len(v) > 0 { // a candidate the provider has not minted yet
			out = append(out, v)
		}
	}
	return out, nil
}

func (e *env) HTTPClient() *http.Client { return e.s.cfg.HTTPClient }

func (e *env) Logger() *slog.Logger {
	return e.s.cfg.Logger.With("provider", e.inst.Provider, "app_instance", e.inst.ID)
}

func (e *env) Now() time.Time { return e.s.cfg.Now() }

func (e *env) WorkspaceURL() *url.URL { u := *e.s.cfg.WorkspaceURL; return &u }

func (e *env) IssuerURL() *url.URL { u := *e.s.cfg.IssuerURL; return &u }

func (e *env) WebhookURL(instanceID uuid.UUID) *url.URL { return e.s.WebhookURL(instanceID) }

// fixedSecrets is an Env whose Credentials returns given values for one
// name (proof by use).
type fixedSecrets struct {
	provider.Env
	name   string
	values [][]byte
}

func (f fixedSecrets) Credentials(ctx context.Context, name string) ([][]byte, error) {
	if name == f.name {
		return f.values, nil
	}
	return f.Env.Credentials(ctx, name)
}
