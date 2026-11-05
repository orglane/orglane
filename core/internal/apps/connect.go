package apps

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

// ConnectReply is a safe browser instruction, never provider credentials.
type ConnectReply struct {
	InstanceID uuid.UUID
	Done       bool
	Step       *provider.ConnectStep
}

// BeginGitHubConnect creates a connecting instance before rendering its manifest.
// Resuming an instance is explicit and keeps its immutable expected organization.
func (s *Service) BeginGitHubConnect(ctx context.Context, actor audit.Actor, personID, sessionID uuid.UUID, org, key string, existing *uuid.UUID, manual url.Values) (ConnectReply, error) {
	if personID == uuid.Nil || sessionID == uuid.Nil {
		return ConnectReply{}, ErrInvalidState
	}
	adapter, ok := s.reg.Adapter("github")
	if !ok {
		return ConnectReply{}, ErrProviderUnavailable
	}
	if _, ok := adapter.impl.(provider.Connector); !ok {
		return ConnectReply{}, ErrNotSupported
	}
	var inst Instance
	var err error
	if existing != nil {
		inst, err = s.Instance(ctx, *existing)
		if err != nil {
			return ConnectReply{}, err
		}
		if inst.Provider != "github" || inst.State != StateConnecting || inst.Key != key || !strings.EqualFold(org, settingString(inst.Settings, "organization")) {
			return ConnectReply{}, ErrInvalidState
		}
	} else {
		if !githubprovider.ValidOrganization(org) {
			return ConnectReply{}, invalid("organization", "must be a GitHub organization login")
		}
		inst, err = s.CreateInstance(ctx, actor, NewInstance{Provider: "github", Key: key, DisplayName: "GitHub · " + org, Settings: map[string]any{"organization": org}})
		if err != nil {
			return ConnectReply{}, err
		}
	}
	held, err := s.holdLock(ctx, lockID("connect", inst.ID.String()))
	if err != nil {
		return ConnectReply{}, err
	}
	defer held.release()
	inst, err = s.loadInstance(ctx, held.queries(), inst.ID, false)
	if err != nil {
		return ConnectReply{}, err
	}
	if inst.State != StateConnecting {
		return ConnectReply{}, ErrInvalidState
	}
	connector := inst.Adapter.impl.(provider.Connector)
	state := crypto.RandomToken("ols_")
	if len(manual) > 0 {
		if settingString(inst.Settings, "app_id") != "" {
			return ConnectReply{}, ErrInvalidState
		}
		manual.Set("state", state)
		result, err := connector.CompleteConnect(ctx, held.env(inst), inst.View(), "manual", manual)
		if err != nil {
			return ConnectReply{}, err
		}
		return s.saveConnect(ctx, held, actor, personID, sessionID, inst, state, result)
	}
	step, err := connector.BeginConnect(ctx, held.env(inst), inst.View(), state)
	if err != nil {
		return ConnectReply{}, err
	}
	return s.saveConnect(ctx, held, actor, personID, sessionID, inst, state, provider.ConnectResult{Next: &step})
}

func settingString(m map[string]any, key string) string { s, _ := m[key].(string); return s }

// CompleteGitHubConnect consumes the state before contacting GitHub, including
// on failure. A code cannot be replayed after a lost response; installation can
// be resumed explicitly with a fresh state, using stored App credentials.
func (s *Service) CompleteGitHubConnect(ctx context.Context, actor audit.Actor, personID, sessionID uuid.UUID, state string, params url.Values) (ConnectReply, error) {
	if personID == uuid.Nil || sessionID == uuid.Nil || !strings.HasPrefix(state, "ols_") || len(state) > 100 {
		return ConnectReply{}, ErrInvalidState
	}
	instanceID, err := store.New(s.pool).GetAppConnectStateInstance(ctx, store.GetAppConnectStateInstanceParams{StateHash: crypto.HashToken(state), PersonID: personID, SessionID: sessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectReply{}, ErrInvalidState
	}
	if err != nil {
		return ConnectReply{}, err
	}
	held, err := s.holdLock(ctx, lockID("connect", instanceID.String()))
	if err != nil {
		return ConnectReply{}, err
	}
	defer held.release()
	row, err := held.queries().ConsumeAppOAuthState(ctx, store.ConsumeAppOAuthStateParams{StateHash: crypto.HashToken(state), Kind: "connect", PersonID: personID, SessionID: sessionID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ConnectReply{}, ErrInvalidState
	}
	if err != nil {
		return ConnectReply{}, err
	}
	inst, err := s.loadInstance(ctx, held.queries(), row.InstanceID, false)
	if err != nil {
		return ConnectReply{}, err
	}
	if inst.Provider != "github" || inst.State != StateConnecting || inst.Adapter == nil || row.Step == nil {
		return ConnectReply{}, ErrInvalidState
	}
	step := *row.Step
	if (step == "create_app" && (params.Get("code") == "" || params.Get("installation_id") != "" || settingString(inst.Settings, "app_id") != "")) || (step == "install_app" && (params.Get("installation_id") == "" || params.Get("code") != "" || settingString(inst.Settings, "app_id") == "")) {
		return ConnectReply{}, ErrInvalidState
	}
	nextState := crypto.RandomToken("ols_")
	params.Set("state", nextState)
	result, err := inst.Adapter.impl.(provider.Connector).CompleteConnect(ctx, held.env(inst), inst.View(), step, params)
	if err != nil {
		return ConnectReply{}, err
	}
	return s.saveConnect(ctx, held, actor, personID, sessionID, inst, nextState, result)
}

// validConnectStep is a second check at the core boundary: provider output may
// only navigate to GitHub.com and cannot smuggle credentials in a URL or form.
func validConnectStep(step *provider.ConnectStep) bool {
	if step == nil || step.URL == nil {
		return false
	}
	u := step.URL
	if u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return false
	}
	if step.Name == "create_app" {
		return step.Method == "POST" && strings.HasPrefix(u.Path, "/organizations/") && strings.HasSuffix(u.Path, "/settings/apps/new") && len(step.Form) == 1 && step.Form["manifest"] != ""
	}
	return step.Name == "install_app" && step.Method == "GET" && strings.HasPrefix(u.Path, "/apps/") && strings.HasSuffix(u.Path, "/installations/new") && len(step.Form) == 0
}

func (s *Service) saveConnect(ctx context.Context, held *heldConn, actor audit.Actor, personID, sessionID uuid.UUID, inst Instance, state string, result provider.ConnectResult) (ConnectReply, error) {
	defer func() {
		for _, v := range result.Credentials {
			clear(v)
		}
	}()
	if !result.Done && !validConnectStep(result.Next) {
		return ConnectReply{}, ErrInvalidState
	}
	if result.Done && (result.Next != nil || result.BaseURL == nil || result.BaseURL.Scheme != "https" || result.BaseURL.Host != "github.com" || result.BaseURL.User != nil || result.BaseURL.RawQuery != "" || result.BaseURL.Fragment != "") {
		return ConnectReply{}, ErrInvalidState
	}
	err := held.inTx(ctx, func(tx pgx.Tx, q *store.Queries) error {
		current, err := s.loadInstance(ctx, q, inst.ID, true)
		if err != nil {
			return err
		}
		if current.State != StateConnecting {
			return ErrInvalidState
		}
		settings := map[string]any{}
		for k, v := range current.Settings {
			settings[k] = v
		}
		for k, v := range result.Settings {
			settings[k] = v
		}
		// These settings come exclusively from the connector, not UpdateInstance.
		settings, err = mergeSettings(inst.Adapter.Manifest.Settings, nil, settings, true)
		if err != nil {
			return err
		}
		raw, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		if _, err = q.UpdateAppInstanceConfig(ctx, store.UpdateAppInstanceConfigParams{ID: inst.ID, DisplayName: current.DisplayName, AccessPolicy: current.AccessPolicy, ManageExistingMembers: current.ManageExistingMembers, Settings: raw}); err != nil {
			return err
		}
		for name, value := range result.Credentials {
			spec, ok := inst.Adapter.Credential(name)
			if !ok || len(value) == 0 {
				return ErrInvalidState
			}
			if err = s.replaceActiveCredential(ctx, q, actor, current, spec, value); err != nil {
				return err
			}
		}
		// A fresh start/resume invalidates all older states for this instance.
		if err = q.InvalidateAppConnectStates(ctx, inst.ID); err != nil {
			return err
		}
		if result.Done {
			if err = q.SetConnectedAppBaseURL(ctx, store.SetConnectedAppBaseURLParams{ID: inst.ID, BaseUrl: ptr(result.BaseURL.String())}); err != nil {
				return err
			}
			_, err = s.transition(ctx, tx, q, actor, inst.ID, StateActive, "")
			return err
		}
		_, err = q.InsertAppOAuthState(ctx, store.InsertAppOAuthStateParams{ID: uuid.Must(uuid.NewV7()), Kind: "connect", StateHash: crypto.HashToken(state), InstanceID: inst.ID, PersonID: personID, SessionID: sessionID, Step: &result.Next.Name, ExpiresAt: s.cfg.Now().Add(time.Hour)})
		if err != nil {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.AppInstanceUpdated, Target: instanceTarget(inst.ID), Metadata: map[string]any{"provider": "github", "connect_step": result.Next.Name}})
		return err
	})
	if err != nil {
		return ConnectReply{}, err
	}
	return ConnectReply{InstanceID: inst.ID, Done: result.Done, Step: result.Next}, nil
}
