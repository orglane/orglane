package apps

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// MaxWebhookBody is the largest webhook body accepted (5 MiB).
const MaxWebhookBody = 5 << 20

// webhookHeaders is the allowlist of stored delivery headers. Headers are
// not signed: they route and display, never authorize or dedupe.
var webhookHeaders = []string{"X-GitHub-Event", "X-GitHub-Delivery", "X-Forgejo-Event", "X-Forgejo-Delivery", "Content-Type", "User-Agent"}

// eventHeaders name the event of a delivery, in order.
var eventHeaders = []string{"X-GitHub-Event", "X-Forgejo-Event"}

// processing are the states in which deliveries are processed.
var processing = []string{StateActive, StateDegraded, StateUpgrading}

// Webhook receipt outcomes.
var (
	// ErrWebhookUnknown: unknown, removed or connecting instance, or one
	// whose provider takes no webhooks (404, the answer does not say which).
	ErrWebhookUnknown = errors.New("apps: no such webhook")
	// ErrWebhookSignature: no candidate secret verifies the signature.
	ErrWebhookSignature = errors.New("apps: webhook signature does not verify")
	// ErrWebhookMalformed: the delivery lacks a usable delivery ID or
	// event name.
	ErrWebhookMalformed = errors.New("apps: malformed webhook delivery")
)

// WebhookReceipt is the result of ReceiveWebhook.
type WebhookReceipt struct {
	// Duplicate: the signed body or delivery ID was seen before; nothing
	// was stored or queued (200). Otherwise the delivery is stored and
	// queued (202).
	Duplicate bool
}

// ReceiveWebhook verifies and stores a webhook delivery and queues its
// processing in the same transaction. It makes no provider API calls and
// does not normalize, so it answers within milliseconds.
func (s *Service) ReceiveWebhook(ctx context.Context, instanceID uuid.UUID, h http.Header, body []byte) (WebhookReceipt, error) {
	q := store.New(s.pool)
	inst, err := s.loadInstance(ctx, q, instanceID, false)
	if errors.Is(err, ErrNotFound) {
		return WebhookReceipt{}, ErrWebhookUnknown
	} else if err != nil {
		return WebhookReceipt{}, err
	}
	if inst.State == StateRemoved || inst.State == StateConnecting || inst.Adapter == nil {
		return WebhookReceipt{}, ErrWebhookUnknown
	}
	src, ok := inst.Adapter.EventSource()
	if !ok {
		return WebhookReceipt{}, ErrWebhookUnknown
	}
	if len(body) > MaxWebhookBody {
		return WebhookReceipt{}, invalid("body", "too large")
	}
	if err := src.Verify(ctx, s.env(inst), inst.View(), h, body); err != nil {
		if !errors.Is(err, provider.ErrBadSignature) {
			// Loading the secrets failed, not the signature: a server error.
			return WebhookReceipt{}, fmt.Errorf("apps: verify webhook: %w", err)
		}
		if derr := s.detectRetainedDrift(ctx, inst, src, h, body); derr != nil {
			s.cfg.Logger.ErrorContext(ctx, "webhook drift check failed", "provider", inst.Provider, "app_instance", inst.ID, "error", derr)
		}
		if s.badSignatures.allow(s.cfg.Now()) {
			// Never the body or headers: they are untrusted and may carry
			// personal data.
			s.cfg.Logger.WarnContext(ctx, "webhook signature did not verify", "provider", inst.Provider, "app_instance", inst.ID)
		}
		return WebhookReceipt{}, ErrWebhookSignature
	}
	sum := sha256.Sum256(body)
	deliveryID, err := src.DeliveryID(h)
	if err != nil || deliveryID == "" {
		// The delivery header is not signed: without it, the signed body
		// identifies the delivery (a replay of it is still a duplicate).
		deliveryID = "body:" + hex.EncodeToString(sum[:16])
	}
	if len(deliveryID) > 100 || !utf8.ValidString(deliveryID) {
		return WebhookReceipt{}, ErrWebhookMalformed
	}
	eventName := ""
	for _, name := range eventHeaders {
		if v := h.Get(name); v != "" {
			eventName = v
			break
		}
	}
	if len(eventName) > 100 || !utf8.ValidString(eventName) {
		return WebhookReceipt{}, ErrWebhookMalformed
	}
	headers := map[string]string{}
	for _, name := range webhookHeaders {
		if v := h.Get(name); v != "" && utf8.ValidString(v) {
			if len(v) > 500 {
				v = v[:500]
			}
			headers[name] = strings.ToValidUTF8(v, "")
		}
	}
	rawHeaders, err := json.Marshal(headers)
	if err != nil {
		return WebhookReceipt{}, fmt.Errorf("apps: encode headers: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return WebhookReceipt{}, fmt.Errorf("apps: delivery id: %w", err)
	}
	var receipt WebhookReceipt
	err = s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		if err := s.classifyDelivery(ctx, tx, q, inst, src, h, body); err != nil {
			return err
		}
		n, err := q.InsertWebhookDelivery(ctx, store.InsertWebhookDeliveryParams{
			ID:         id,
			InstanceID: inst.ID,
			DeliveryID: deliveryID,
			EventName:  eventName,
			ReceivedAt: s.cfg.Now().UTC(),
			Body:       s.deliveries.Seal(body, deliveryAD(inst.ID, id)),
			Headers:    rawHeaders,
			BodySha256: sum[:],
		})
		if err != nil {
			return fmt.Errorf("apps: store delivery: %w", err)
		}
		if n == 0 {
			receipt.Duplicate = true
			return nil
		}
		return s.enqueue(ctx, tx, ProcessDeliveryArgs{DeliveryID: id})
	})
	return receipt, err
}

func deliveryAD(instanceID, rowID uuid.UUID) []byte {
	return []byte(instanceID.String() + "\x00" + rowID.String())
}

// Delivery statuses.
const (
	deliveryReceived  = "received"
	deliveryProcessed = "processed"
	deliveryIgnored   = "ignored"
	deliveryFailed    = "failed"
)

// codeEffectReplayed marks a delivery whose signed body's effects were
// already considered for this instance, before its delivery row expired.
const codeEffectReplayed = "effect_replayed"

var (
	eventTypePattern = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*){2}$`)
	// maxEventsPerDelivery bounds what one delivery may create.
	maxEventsPerDelivery = 100
)

// RunProcessDelivery opens a stored delivery, normalizes it and stores its
// events, all in one transaction. Normalization errors are permanent;
// database errors retry.
func (s *Service) RunProcessDelivery(ctx context.Context, args ProcessDeliveryArgs) error {
	return s.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		row, err := q.GetWebhookDeliveryForUpdate(ctx, args.DeliveryID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return fmt.Errorf("apps: load delivery: %w", err)
		}
		if row.Status != deliveryReceived {
			return nil
		}
		finish := func(status, code string, n int) error {
			var c *string
			if code != "" {
				c = &code
			}
			if err := q.FinishWebhookDelivery(ctx, store.FinishWebhookDeliveryParams{ID: row.ID, Status: status, ErrorCode: c, EventCount: int32(n)}); err != nil { //nolint:gosec // bounded
				return fmt.Errorf("apps: finish delivery: %w", err)
			}
			return nil
		}
		inst, err := s.loadInstance(ctx, q, row.InstanceID, false)
		if err != nil {
			return err
		}
		if inst.Adapter == nil {
			return finish(deliveryFailed, "provider_unavailable", 0)
		}
		src, ok := inst.Adapter.EventSource()
		if !ok {
			return finish(deliveryFailed, "provider_unavailable", 0)
		}
		if !inInstanceStates(inst, processing) {
			return finish(deliveryIgnored, "instance_inactive", 0)
		}
		if row.Body == nil {
			return finish(deliveryFailed, "body_expired", 0)
		}
		body, err := s.deliveries.Open(row.Body, deliveryAD(row.InstanceID, row.ID))
		if err != nil {
			// Permanent (a wrong key or a damaged row): a terminal status
			// operators see, not a delivery that looks unprocessed.
			s.cfg.Logger.ErrorContext(ctx, "webhook delivery body cannot be opened", "delivery", row.ID, "error", err)
			return finish(deliveryFailed, "body_unreadable", 0)
		}
		var stored map[string]string
		if err := json.Unmarshal(row.Headers, &stored); err != nil {
			s.cfg.Logger.ErrorContext(ctx, "webhook delivery headers cannot be decoded", "delivery", row.ID, "error", err)
			return finish(deliveryFailed, "headers_unreadable", 0)
		}
		h := http.Header{}
		for k, v := range stored {
			h.Set(k, v)
		}
		d := provider.Delivery{ID: row.DeliveryID, EventName: row.EventName, Headers: h, Body: body, ReceivedAt: row.ReceivedAt}
		effected, replayed := false, false
		if fx, ok := src.(provider.DeliveryEffects); ok {
			effect, err := fx.Effects(inst.View(), d)
			if err != nil {
				status, code := normalizeFailure(err)
				return finish(status, code, 0)
			}
			// The signed body, not the delivery row, is the durable effect
			// key: delivery rows expire after 30 days, but a replay must
			// never reapply a connection effect (docs/specs/providers.md
			// "Replay and retention").
			kind, code := effectMark(effect)
			inserted, err := q.InsertWebhookEffectMark(ctx, store.InsertWebhookEffectMarkParams{
				InstanceID:   row.InstanceID,
				BodySha256:   row.BodySha256,
				Effect:       kind,
				Code:         code,
				ConsideredAt: s.cfg.Now().UTC(),
			})
			if err != nil {
				return fmt.Errorf("apps: mark delivery effect: %w", err)
			}
			if inserted == 0 {
				replayed = true
			} else if effected, err = s.applyEffect(ctx, tx, q, inst, effect); err != nil {
				return err
			}
		}
		events, err := src.Normalize(inst.View(), d)
		switch {
		case err != nil:
			status, code := normalizeFailure(err)
			return finish(status, code, 0)
		case events == nil && effected:
			return finish(deliveryProcessed, "", 0)
		case events == nil && replayed:
			return finish(deliveryIgnored, codeEffectReplayed, 0)
		case events == nil:
			return finish(deliveryIgnored, "not_normalized", 0)
		}
		if len(events) > maxEventsPerDelivery {
			return finish(deliveryFailed, "too_many_events", 0)
		}
		capabilities := inst.Adapter.Manifest.Capabilities
		params := make([]store.InsertEventParams, 0, len(events))
		for _, e := range events {
			p, err := eventParams(inst, row, e, capabilities)
			if err != nil {
				return finish(deliveryFailed, "invalid_event", 0)
			}
			params = append(params, p)
		}
		n := 0
		for _, p := range params {
			created, err := q.InsertEvent(ctx, p)
			if err != nil {
				return fmt.Errorf("apps: store event: %w", err)
			}
			n += int(created)
		}
		return finish(deliveryProcessed, "", n)
	})
}

// normalizeFailure maps an error of Normalize or Effects to the delivery's
// final status and code: a foreign delivery is ignored, anything else
// failed for good.
func normalizeFailure(err error) (status, code string) {
	if errors.Is(err, provider.ErrForeignDelivery) {
		code = "foreign_delivery"
		var pe *provider.Error
		if errors.As(err, &pe) && pe.Code != "" {
			code = pe.Code
		}
		return deliveryIgnored, code
	}
	code, _, _ = provider.Classify(err)
	if code == provider.CodeProviderError {
		code = "normalize_failed"
	}
	return deliveryFailed, code
}

var effectCodePattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// effectMark names an effect for its durable mark. Every delivery whose
// effects were evaluated gets a mark, including one that changes nothing:
// the mark says "considered once", so a replay cannot turn a no-op, an
// already satisfied state or a skipped transition into a later state change.
// An effect whose state or code is invalid is skipped by applyEffect and is
// recorded as no effect (or as only its resync).
func effectMark(e provider.Effect) (kind, code string) {
	switch e.State {
	case provider.EffectDegraded, provider.EffectDisconnected, provider.EffectRestored:
		if effectCodePattern.MatchString(e.Code) {
			return e.State, e.Code
		}
	}
	if e.Resync {
		return "resync", ""
	}
	return "none", ""
}

// applyEffect applies what a delivery changes besides its events, in the
// transaction that stores them (docs/specs/providers.md "Webhooks" ->
// "Processing"). State changes are audited as system transitions; a change
// the instance's current state does not allow is skipped. It reports
// whether anything was changed or queued.
func (s *Service) applyEffect(ctx context.Context, tx pgx.Tx, q *store.Queries, inst Instance, e provider.Effect) (bool, error) {
	changed := false
	if e.State != "" {
		if !effectCodePattern.MatchString(e.Code) {
			return false, nil
		}
		current, err := s.loadInstance(ctx, q, inst.ID, true)
		if err != nil {
			return false, err
		}
		to, code := "", e.Code
		switch e.State {
		case provider.EffectDegraded:
			to = StateDegraded
		case provider.EffectDisconnected:
			to = StateDisconnected
		case provider.EffectRestored:
			if current.State == StateDegraded && derefString(current.HealthCode) == e.Code {
				to, code = StateActive, ""
			}
		}
		if to != "" && (current.State != to || derefString(current.HealthCode) != code) {
			_, err := s.transition(ctx, tx, q, audit.System, inst.ID, to, code)
			switch {
			case errors.Is(err, ErrInvalidState):
			case err != nil:
				return false, err
			default:
				changed = true
			}
		}
		if inst, err = s.loadInstance(ctx, q, inst.ID, false); err != nil {
			return false, err
		}
	}
	if e.Resync && inst.Provisionable() {
		if _, ok := inst.Adapter.Provisioner(); ok {
			if err := s.enqueueInstance(ctx, tx, inst.ID, ReasonDrift); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

func inInstanceStates(inst Instance, states []string) bool {
	for _, st := range states {
		if inst.State == st {
			return true
		}
	}
	return false
}

// eventParams validates an adapter's event and maps it to a row. Types are
// prefixed by one of the provider's capabilities; dedupe keys come from the
// adapter (signed body fields).
func eventParams(inst Instance, row store.WebhookDelivery, e provider.Event, capabilities []provider.Capability) (store.InsertEventParams, error) {
	if !eventTypePattern.MatchString(e.Type) {
		return store.InsertEventParams{}, errors.New("apps: invalid event type")
	}
	prefixed := false
	for _, c := range capabilities {
		if strings.HasPrefix(e.Type, string(c)+".") {
			prefixed = true
		}
	}
	if !prefixed || e.SchemaVersion <= 0 || e.DedupeKey == "" || len(e.DedupeKey) > 500 || !utf8.ValidString(e.DedupeKey) {
		return store.InsertEventParams{}, errors.New("apps: invalid event")
	}
	occurred := e.OccurredAt
	if occurred.IsZero() {
		occurred = row.ReceivedAt
	}
	targets := e.Targets
	if targets == nil {
		targets = []provider.Target{}
	}
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	actor, err1 := json.Marshal(e.Actor)
	subject, err2 := json.Marshal(e.Subject)
	rawTargets, err3 := json.Marshal(targets)
	rawData, err4 := json.Marshal(data)
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return store.InsertEventParams{}, err
	}
	return store.InsertEventParams{
		Type:          e.Type,
		SchemaVersion: int32(e.SchemaVersion), //nolint:gosec // small
		InstanceID:    inst.ID,
		Provider:      inst.Provider,
		DeliveryID:    &row.ID,
		DedupeKey:     e.DedupeKey,
		OccurredAt:    occurred.UTC(),
		ReceivedAt:    row.ReceivedAt,
		Actor:         actor,
		Subject:       subject,
		Targets:       rawTargets,
		Data:          rawData,
	}, nil
}

// CleanupResult counts what Cleanup removed.
type CleanupResult struct {
	Credentials, Calls, OAuthStates, Bodies, Deliveries int64
}

// Cleanup is the daily retention job: retired and discarded credential
// versions, provider call records a day after their lease, expired OAuth
// state, delivery bodies 7 days after processing
// (30 for failed ones) and deliveries after 30 days. Events and the
// durable effect marks are never touched: they outlive delivery retention
// so a replay cannot reapply an effect. It also reports rotations
// uncertain for a day.
func (s *Service) Cleanup(ctx context.Context) (CleanupResult, error) {
	q := store.New(s.pool)
	var r CleanupResult
	var err error
	if r.Credentials, err = q.DeleteExpiredAppCredentials(ctx); err != nil {
		return r, fmt.Errorf("apps: delete credentials: %w", err)
	}
	if r.Calls, err = q.DeleteExpiredAppCredentialCalls(ctx); err != nil {
		return r, fmt.Errorf("apps: delete provider call records: %w", err)
	}
	if r.OAuthStates, err = q.DeleteExpiredAppOAuthStates(ctx); err != nil {
		return r, fmt.Errorf("apps: delete oauth states: %w", err)
	}
	if r.Bodies, err = q.ClearWebhookDeliveryBodies(ctx); err != nil {
		return r, fmt.Errorf("apps: clear delivery bodies: %w", err)
	}
	if r.Deliveries, err = q.DeleteOldWebhookDeliveries(ctx); err != nil {
		return r, fmt.Errorf("apps: delete deliveries: %w", err)
	}
	stuck, err := q.ListStuckAppCredentials(ctx, s.cfg.Now().Add(-stuckAfter))
	if err != nil {
		return r, fmt.Errorf("apps: list stuck rotations: %w", err)
	}
	for _, c := range stuck {
		inst, err := s.loadInstance(ctx, q, c.InstanceID, false)
		if err != nil {
			return r, err
		}
		if err := s.reportStuck(ctx, s.pool, inst, c); err != nil {
			return r, err
		}
	}
	return r, nil
}

// RunSetup works one attempt of the setup job: Lifecycle.Setup of a managed
// instance, then its webhooks (ensureWebhooks). Setup runs after an install
// and after the last upgrade step or the revert spec of a rollback; success
// activates it, exhausted retries fail it with setup_failed. A provider's
// RetryAfter snoozes the job without spending an attempt, also on the last
// attempt.
func (s *Service) RunSetup(ctx context.Context, args SetupArgs, attempt, maxAttempts int) error {
	inst, err := s.Instance(ctx, args.InstanceID)
	if errors.Is(err, ErrNotFound) {
		return river.JobCancel(err)
	} else if err != nil {
		return err
	}
	if (inst.State != StateInstalling && inst.State != StateUpgrading) || inst.Adapter == nil {
		return nil
	}
	if args.Generation == 0 {
		// A setup job queued before upgrades carried a generation never
		// belongs to an upgrade: it may resume after the instance started
		// one and must not activate or fail it there.
		if inst.State != StateInstalling || inst.DesiredGeneration != inst.ObservedGeneration {
			return nil
		}
	} else if inst.DesiredGeneration != args.Generation {
		// A newer generation superseded this setup; its consequences run
		// their own setup.
		return nil
	}
	if inst.DesiredGeneration != inst.ObservedGeneration {
		// The agent has not applied the current generation yet (for example
		// a resync after it lost the state). Running Setup now would
		// activate the instance before that apply can fail it.
		return nil
	}
	state := inst.State
	lc, ok := inst.Adapter.Lifecycle()
	if !ok {
		return river.JobCancel(ErrNotSupported)
	}
	setupErr := lc.Setup(ctx, s.env(inst), inst.View())
	if setupErr == nil {
		// The provider's webhooks are part of a set-up instance (a Setup
		// after an upgrade or restore also brings back the hook and its
		// active secret).
		setupErr = s.ensureWebhooks(ctx, inst, state)
	}
	if errors.Is(setupErr, errWebhooksDeferred) {
		// A rotation of the webhook secret is in progress; its job sets the
		// hook with the value that wins.
		setupErr = nil
	}
	if setupErr == nil {
		// An installed instance is active only once Traefik serves its
		// public route (docs/specs/providers.md "Route publication").
		setupErr = s.awaitRoute(ctx, inst)
	}
	if errors.Is(setupErr, errStateChanged) {
		// No longer installing or upgrading (an agent operation failed it,
		// it is being removed): neither activated nor failed here.
		return nil
	}
	if setupErr == nil {
		return s.finishSetup(ctx, inst.ID, args.Generation, nil)
	}
	code, retryable, retryAfter := provider.Classify(setupErr)
	s.env(inst).Logger().WarnContext(ctx, "setup failed", "code", code, "attempt", attempt)
	switch {
	case retryable && retryAfter > 0:
		return river.JobSnooze(retryAfter)
	case retryable && attempt < maxAttempts:
		return fmt.Errorf("apps: setup failed (%s)", code)
	}
	return s.finishSetup(ctx, inst.ID, args.Generation, setupErr)
}
