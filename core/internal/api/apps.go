package api

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
	"github.com/orglane/orglane/providers/provider"
)

// appsService returns the provider framework.
func (s *server) appsService() (*apps.Service, error) {
	if s.opts.Apps == nil {
		return nil, errors.New("api: the provider framework is not configured")
	}
	return s.opts.Apps, nil
}

// appsManager returns the provider framework and the audit actor of an
// admin with apps.manage (checked by the route guard).
func (s *server) appsManager(ctx context.Context) (*apps.Service, audit.Actor, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, audit.Actor{}, err
	}
	_, actor, err := s.manager(ctx)
	if err != nil {
		return nil, audit.Actor{}, err
	}
	return svc, actor, nil
}

// appsError maps provider framework errors to problem details with a
// stable code.
func appsError(err error) error {
	code := apps.Code(err)
	switch {
	case errors.Is(err, apps.ErrNotFound):
		return codedProblem(http.StatusNotFound, code, "")
	case errors.Is(err, apps.ErrInvalid):
		return codedProblem(http.StatusBadRequest, code, strings.TrimPrefix(err.Error(), "apps: invalid input: "))
	case errors.Is(err, apps.ErrForbidden):
		return codedProblem(http.StatusForbidden, code, "")
	case code != "":
		return codedProblem(http.StatusConflict, code, strings.TrimPrefix(err.Error(), "apps: "))
	}
	return err
}

func (s *server) ListAppProviders(context.Context, apigen.ListAppProvidersRequestObject) (apigen.ListAppProvidersResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	items := []apigen.AppProvider{}
	for _, a := range svc.Registry().Adapters() {
		items = append(items, appProvider(a))
	}
	return apigen.ListAppProviders200JSONResponse{Items: items}, nil
}

func appProvider(a *apps.Adapter) apigen.AppProvider {
	m := a.Manifest
	p := apigen.AppProvider{
		Id:            string(m.ID),
		Name:          m.Name,
		Capabilities:  []string{},
		Management:    apigen.AppManagement(m.Management),
		Features:      []apigen.AppFeatureSupport{},
		Credentials:   []apigen.AppCredentialSpec{},
		Settings:      []apigen.AppSettingSpec{},
		EmployeeSteps: []apigen.AppEmployeeStep{},
		DefaultKey:    m.DefaultKey,
		DefaultPolicy: apigen.AppAccessPolicy(m.DefaultPolicy),
	}
	_, p.Provisioning = a.Provisioner()
	_, p.ExistingTeams = a.TeamLister()
	_, p.Webhooks = a.EventSource()
	_, p.Lifecycle = a.Lifecycle()
	if m.Package != "" {
		p.Package = &m.Package
	}
	for _, c := range m.Capabilities {
		p.Capabilities = append(p.Capabilities, string(c))
	}
	// Matrix order, every feature.
	for _, f := range provider.Features {
		for _, fs := range m.Features {
			if fs.Feature != f {
				continue
			}
			cell := apigen.AppFeatureSupport{Feature: string(fs.Feature), Support: apigen.AppFeatureSupportSupport(fs.Support)}
			if fs.Note != "" {
				cell.Note = &fs.Note
			}
			if fs.Milestone != "" {
				cell.Milestone = &fs.Milestone
			}
			p.Features = append(p.Features, cell)
		}
	}
	for _, c := range m.Credentials {
		p.Credentials = append(p.Credentials, apigen.AppCredentialSpec{Name: c.Name, Source: apigen.AppCredentialSpecSource(c.Source), Rotatable: c.Rotatable, Agent: c.Agent})
	}
	for _, st := range m.Settings {
		spec := apigen.AppSettingSpec{Name: st.Name, Type: apigen.AppSettingSpecType(st.Type), Mutable: st.Mutable, Required: st.Required, Default: st.Default}
		if st.Pattern != "" {
			spec.Pattern = &st.Pattern
		}
		if len(st.Enum) > 0 {
			spec.Enum = &st.Enum
		}
		if st.Type == provider.SettingInt {
			spec.Min, spec.Max = &st.Min, &st.Max
		}
		p.Settings = append(p.Settings, spec)
	}
	for _, step := range m.EmployeeSteps {
		p.EmployeeSteps = append(p.EmployeeSteps, apigen.AppEmployeeStep{Id: step.ID, Title: step.Title, Description: step.Description, ActionPath: step.ActionPath, Required: step.Required})
	}
	return p
}

func appInstance(svc *apps.Service, inst apps.Instance) apigen.AppInstance {
	out := apigen.AppInstance{
		Id:                    inst.ID,
		Key:                   inst.Key,
		Provider:              inst.Provider,
		ProviderAvailable:     inst.Adapter != nil,
		Management:            apigen.AppManagement(inst.Management),
		DisplayName:           inst.DisplayName,
		State:                 apigen.AppInstanceState(inst.State),
		Health:                apigen.AppHealth(inst.Health),
		HealthCode:            inst.HealthCode,
		HealthCheckedAt:       inst.HealthCheckedAt,
		BaseUrl:               inst.BaseUrl,
		AccessPolicy:          apigen.AppAccessPolicy(inst.AccessPolicy),
		ManageExistingMembers: inst.ManageExistingMembers,
		PackageId:             inst.PackageID,
		PackageVersion:        inst.PackageVersion,
		Settings:              inst.Settings,
		DataRetained:          inst.DataRetained,
		CreatedAt:             inst.CreatedAt,
		UpdatedAt:             inst.UpdatedAt,
		RemovedAt:             inst.RemovedAt,
	}
	if inst.WaitingForAgent != "" {
		w := apigen.AppInstanceWaitingForAgent(inst.WaitingForAgent)
		out.WaitingForAgent = &w
	}
	if out.Settings == nil {
		out.Settings = map[string]any{}
	}
	if inst.Adapter != nil {
		if _, ok := inst.Adapter.EventSource(); ok || inst.Provider == "github" {
			u := svc.WebhookURL(inst.ID).String()
			out.WebhookUrl = &u
		}
	}
	return out
}

func (s *server) ListAppInstances(ctx context.Context, _ apigen.ListAppInstancesRequestObject) (apigen.ListAppInstancesResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	rows, err := svc.Instances(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.AppInstance, 0, len(rows))
	for _, inst := range rows {
		items = append(items, appInstance(svc, inst))
	}
	return apigen.ListAppInstances200JSONResponse{Items: items}, nil
}

func (s *server) GetAppInstance(ctx context.Context, req apigen.GetAppInstanceRequestObject) (apigen.GetAppInstanceResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	inst, err := svc.Instance(ctx, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	d, err := svc.Detail(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	out := apigen.GetAppInstance200JSONResponse{
		Instance:              appInstance(svc, inst),
		Operations:            []apigen.AppOperation{},
		Credentials:           []apigen.AppCredentialVersion{},
		CredentialRotations:   []apigen.AppCredentialRotationStatus{},
		RotationStuck:         apps.RotationStuck(d.Credentials, time.Now()),
		UnmanagedAccountCount: int(d.UnmanagedCount),
		Provisioning:          counts(d.StatusCounts),
	}
	out.Bindings = make([]struct {
		Capability string `json:"capability"`
		IsDefault  bool   `json:"isDefault"`
	}, 0, len(d.Bindings))
	for _, b := range d.Bindings {
		out.Bindings = append(out.Bindings, struct {
			Capability string `json:"capability"`
			IsDefault  bool   `json:"isDefault"`
		}{Capability: b.Capability, IsDefault: b.IsDefault})
	}
	for _, op := range d.Operations {
		out.Operations = append(out.Operations, appOperation(op))
	}
	for _, r := range d.CredentialRotations {
		out.CredentialRotations = append(out.CredentialRotations, apigen.AppCredentialRotationStatus{Name: r.Name, Version: int(r.Version), State: apigen.AppCredentialRotationStatusState(r.State)})
	}
	out.CredentialDrift = make([]struct {
		DetectedAt time.Time `json:"detectedAt"`
		Name       string    `json:"name"`
		Version    int       `json:"version"`
	}, 0, len(d.CredentialDrift))
	for _, r := range d.CredentialDrift {
		out.CredentialDrift = append(out.CredentialDrift, struct {
			DetectedAt time.Time `json:"detectedAt"`
			Name       string    `json:"name"`
			Version    int       `json:"version"`
		}{DetectedAt: r.DetectedAt.UTC(), Name: r.Name, Version: int(r.Version)})
	}
	for _, c := range d.Credentials {
		out.Credentials = append(out.Credentials, apigen.AppCredentialVersion{
			Name: c.Name, Version: int(c.Version), State: apigen.AppCredentialVersionState(c.State), CreatedAt: c.CreatedAt, ActivatedAt: c.ActivatedAt, RetireAt: c.RetireAt,
		})
	}
	return out, nil
}

func appOperation(op apps.Operation) apigen.AppOperation {
	return apigen.AppOperation{
		Id: op.ID, Generation: op.Generation, Kind: apigen.AppOperationKind(op.Kind), Reason: (*apigen.AppOperationReason)(op.Reason),
		State: apigen.AppOperationState(op.State), Phase: op.Phase, ErrorCode: op.ErrorCode, FromVersion: op.FromVersion, ToVersion: op.ToVersion,
		CreatedAt: op.CreatedAt, FinishedAt: op.FinishedAt,
	}
}

func (s *server) ListAppInstanceOperations(ctx context.Context, req apigen.ListAppInstanceOperationsRequestObject) (apigen.ListAppInstanceOperationsResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	limit, offset := 20, 0
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if req.Params.Offset != nil {
		offset = *req.Params.Offset
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1_000_000 {
		return nil, problemErr(http.StatusBadRequest, "limit must be 1-100 and offset 0-1000000.")
	}
	if _, err := svc.Instance(ctx, req.InstanceId); err != nil {
		return nil, appsError(err)
	}
	// One more than the page tells whether more follow.
	rows, err := svc.Operations(ctx, req.InstanceId, int32(limit+1), int32(offset)) //nolint:gosec // both bounded above
	if err != nil {
		return nil, err
	}
	out := apigen.ListAppInstanceOperations200JSONResponse{Items: []apigen.AppOperation{}, HasMore: len(rows) > limit}
	for _, op := range rows[:min(len(rows), limit)] {
		out.Items = append(out.Items, appOperation(op))
	}
	return out, nil
}

func counts(m map[string]int64) apigen.AppProvisioningCounts {
	return apigen.AppProvisioningCounts{
		Queued:                int(m[apps.StatusQueued]),
		Done:                  int(m[apps.StatusDone]),
		PendingEmployeeAction: int(m[apps.StatusPendingEmployeeAction]),
		Failed:                int(m[apps.StatusFailed]),
	}
}

func (s *server) UpdateAppInstance(ctx context.Context, req apigen.UpdateAppInstanceRequestObject) (apigen.UpdateAppInstanceResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := apps.InstanceUpdate{DisplayName: b.DisplayName, ManageExistingMembers: b.ManageExistingMembers}
	if b.AccessPolicy != nil {
		p := provider.AccessPolicy(*b.AccessPolicy)
		in.AccessPolicy = &p
	}
	if b.Settings != nil {
		in.Settings = *b.Settings
	}
	inst, err := svc.UpdateInstance(ctx, actor, req.InstanceId, in)
	if err != nil {
		return nil, appsError(err)
	}
	return apigen.UpdateAppInstance200JSONResponse(appInstance(svc, inst)), nil
}

func (s *server) RetryAppInstanceSetup(ctx context.Context, req apigen.RetryAppInstanceSetupRequestObject) (apigen.RetryAppInstanceSetupResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	inst, err := svc.RetrySetup(ctx, actor, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	return apigen.RetryAppInstanceSetup202JSONResponse(appInstance(svc, inst)), nil
}

func (s *server) RotateAppCredential(ctx context.Context, req apigen.RotateAppCredentialRequestObject) (apigen.RotateAppCredentialResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	version, err := svc.RotateCredential(ctx, actor, req.InstanceId, req.CredentialName)
	if err != nil {
		return nil, appsError(err)
	}
	return apigen.RotateAppCredential202JSONResponse{Name: req.CredentialName, Version: int(version)}, nil
}

func (s *server) AbandonAppCredentialRotation(ctx context.Context, req apigen.AbandonAppCredentialRotationRequestObject) (apigen.AbandonAppCredentialRotationResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	if err := svc.AbandonRotation(ctx, actor, req.InstanceId, req.CredentialName); err != nil {
		return nil, appsError(err)
	}
	return apigen.AbandonAppCredentialRotation202Response{}, nil
}

func (s *server) ResolveAppCredentialRotation(ctx context.Context, req apigen.ResolveAppCredentialRotationRequestObject) (apigen.ResolveAppCredentialRotationResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	resolution := "" // a missing body is invalid input
	if req.Body != nil {
		resolution = string(req.Body.Resolution)
	}
	if err := svc.ResolveRotation(ctx, actor, req.InstanceId, req.CredentialName, resolution); err != nil {
		return nil, appsError(err)
	}
	return apigen.ResolveAppCredentialRotation204Response{}, nil
}

func bindingList(rows []apps.Binding, capability string) apigen.AppBindingList {
	items := []apigen.AppBinding{}
	for _, b := range rows {
		if capability != "" && b.Capability != capability {
			continue
		}
		items = append(items, apigen.AppBinding{
			Capability: b.Capability, InstanceId: b.InstanceID, InstanceKey: b.InstanceKey, InstanceDisplayName: b.InstanceDisplayName,
			Provider: b.InstanceProvider, InstanceState: apigen.AppInstanceState(b.InstanceState), IsDefault: b.IsDefault,
		})
	}
	return apigen.AppBindingList{Items: items}
}

func (s *server) ListAppBindings(ctx context.Context, _ apigen.ListAppBindingsRequestObject) (apigen.ListAppBindingsResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	rows, err := svc.Bindings(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.ListAppBindings200JSONResponse(bindingList(rows, "")), nil
}

func (s *server) SetDefaultAppBinding(ctx context.Context, req apigen.SetDefaultAppBindingRequestObject) (apigen.SetDefaultAppBindingResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	if err := svc.SetDefaultBinding(ctx, actor, req.Capability, req.Body.InstanceId); err != nil {
		return nil, appsError(err)
	}
	rows, err := svc.Bindings(ctx)
	if err != nil {
		return nil, err
	}
	return apigen.SetDefaultAppBinding200JSONResponse(bindingList(rows, req.Capability)), nil
}

func teamMapping(m apps.TeamMapping) apigen.AppTeamMapping {
	return apigen.AppTeamMapping{
		TeamId: m.TeamID, TeamSlug: m.TeamSlug, TeamName: m.TeamName, TeamArchived: m.TeamArchivedAt != nil,
		Mode: apigen.AppTeamMappingMode(m.Mode), ExternalRef: m.ExternalRef, ExternalName: m.ExternalName,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
	}
}

func (s *server) ListAppTeamMappings(ctx context.Context, req apigen.ListAppTeamMappingsRequestObject) (apigen.ListAppTeamMappingsResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	rows, err := svc.TeamMappings(ctx, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	items := make([]apigen.AppTeamMapping, 0, len(rows))
	for _, m := range rows {
		items = append(items, teamMapping(m))
	}
	return apigen.ListAppTeamMappings200JSONResponse{Items: items}, nil
}

func (s *server) ListAppProviderTeams(ctx context.Context, req apigen.ListAppProviderTeamsRequestObject) (apigen.ListAppProviderTeamsResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	teams, err := svc.ProviderTeams(ctx, req.InstanceId)
	if err != nil {
		var pe *provider.Error
		switch {
		case errors.Is(err, apps.ErrNotSupported):
			return nil, codedProblem(http.StatusNotFound, apps.Code(err), "This provider has no existing teams to pick.")
		case errors.As(err, &pe) && pe.RetryAfter > 0:
			return nil, &apiError{status: http.StatusTooManyRequests, code: pe.Code, detail: "The provider's rate limit was reached. Try again later.", retryAfter: seconds(pe.RetryAfter.Seconds())}
		case errors.As(err, &pe):
			return nil, codedProblem(http.StatusBadGateway, pe.Code, "The provider's teams could not be read. Try again.")
		}
		return nil, appsError(err)
	}
	items := make([]apigen.AppProviderTeam, 0, len(teams))
	for _, t := range teams {
		items = append(items, apigen.AppProviderTeam{ExternalRef: t.ExternalRef, Name: t.Name, Slug: t.Slug, Managed: t.Managed})
	}
	return apigen.ListAppProviderTeams200JSONResponse{Items: items}, nil
}

func (s *server) SetAppTeamMapping(ctx context.Context, req apigen.SetAppTeamMappingRequestObject) (apigen.SetAppTeamMappingResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	in := apps.TeamMappingInput{Mode: string(req.Body.Mode)}
	if req.Body.ExternalRef != nil {
		in.ExternalRef = *req.Body.ExternalRef
	}
	if req.Body.ExternalName != nil {
		in.ExternalName = *req.Body.ExternalName
	}
	if _, err := svc.SetTeamMapping(ctx, actor, req.InstanceId, req.TeamId, in); err != nil {
		return nil, appsError(err)
	}
	rows, err := svc.TeamMappings(ctx, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	for _, m := range rows {
		if m.TeamID == req.TeamId {
			return apigen.SetAppTeamMapping200JSONResponse(teamMapping(m)), nil
		}
	}
	return nil, errors.New("api: the team mapping vanished")
}

func (s *server) RemoveAppTeamMapping(ctx context.Context, req apigen.RemoveAppTeamMappingRequestObject) (apigen.RemoveAppTeamMappingResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	if err := svc.RemoveTeamMapping(ctx, actor, req.InstanceId, req.TeamId); err != nil {
		return nil, appsError(err)
	}
	return apigen.RemoveAppTeamMapping204Response{}, nil
}

func actionResults(raw []byte) []apigen.ProvisioningActionResult {
	var rs []apps.ActionResult
	_ = json.Unmarshal(raw, &rs) // written by the framework
	out := make([]apigen.ProvisioningActionResult, 0, len(rs))
	for _, r := range rs {
		a := apigen.ProvisioningActionResult{Kind: string(r.Kind), Key: r.Key, Outcome: apigen.ProvisioningActionResultOutcome(r.Outcome), Removal: r.Removal}
		if r.Error != "" {
			e := r.Error
			a.ErrorCode = &e
		}
		out = append(out, a)
	}
	return out
}

func card(id uuid.UUID, username, displayName, status string) apigen.PersonCard {
	return apigen.PersonCard{Id: id, Username: username, DisplayName: displayName, Status: apigen.PersonStatus(status)}
}

func (s *server) GetAppProvisioning(ctx context.Context, req apigen.GetAppProvisioningRequestObject) (apigen.GetAppProvisioningResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	limit, offset := 50, 0
	if req.Params.Limit != nil {
		limit = *req.Params.Limit
	}
	if req.Params.Offset != nil {
		offset = *req.Params.Offset
	}
	if limit < 1 || limit > 200 || offset < 0 || offset > math.MaxInt32 {
		return nil, problemErr(http.StatusBadRequest, "limit must be 1-200 and offset 0-2147483647.")
	}
	rows, err := svc.Results(ctx, req.InstanceId, int32(limit), int32(offset)) //nolint:gosec // both bounded above
	if err != nil {
		return nil, appsError(err)
	}
	d, err := svc.Detail(ctx, req.InstanceId)
	if err != nil {
		return nil, err
	}
	out := apigen.GetAppProvisioning200JSONResponse{
		Items:             make([]apigen.ProvisioningResult, 0, len(rows)),
		Counts:            counts(d.StatusCounts),
		UnmanagedAccounts: []apigen.UnmanagedAccount{},
	}
	for _, r := range rows {
		out.Items = append(out.Items, apigen.ProvisioningResult{
			Person: card(r.PersonID, r.Username, r.DisplayName, r.PersonStatus), Status: apigen.ProvisioningStatus(r.Status), Access: r.Access,
			EmployeeStep: r.EmployeeStep, GrantedByOrglane: r.GrantedByOrglane, Actions: actionResults(r.Actions), ErrorCode: r.ErrorCode,
			Attempts: int(r.Attempts), LastAttemptAt: r.LastAttemptAt, UpdatedAt: r.UpdatedAt,
		})
	}
	unmanaged, err := svc.UnmanagedAccounts(ctx, req.InstanceId)
	if err != nil {
		return nil, err
	}
	for _, u := range unmanaged {
		out.UnmanagedAccounts = append(out.UnmanagedAccounts, apigen.UnmanagedAccount{ExternalId: u.ExternalID, Login: u.Login, SeenAt: u.SeenAt})
	}
	held, err := svc.HeldRemovals(ctx, req.InstanceId)
	if err != nil {
		return nil, err
	}
	if held != nil {
		b := apigen.HeldRemovalBatch{Id: held.Batch.ID, ItemCount: int(held.Batch.ItemCount), ItemsHash: hex.EncodeToString(held.Batch.ItemsHash), CreatedAt: held.Batch.CreatedAt, Items: []apigen.HeldRemoval{}}
		for _, it := range held.Items {
			var a provider.Action
			_ = json.Unmarshal(it.Action, &a)
			b.Items = append(b.Items, apigen.HeldRemoval{Id: it.ID, Person: card(it.PersonID, it.Username, it.DisplayName, it.PersonStatus), Kind: string(a.Kind), Key: a.Key})
		}
		out.HeldRemovals = &b
	}
	return out, nil
}

func (s *server) PlanAppProvisioning(ctx context.Context, req apigen.PlanAppProvisioningRequestObject) (apigen.PlanAppProvisioningResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	people, truncated, err := svc.DryRun(ctx, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	out := apigen.PlanAppProvisioning200JSONResponse{Items: make([]apigen.PlannedPerson, 0, len(people)), Truncated: truncated}
	for _, p := range people {
		item := apigen.PlannedPerson{Person: card(p.Person.ID, p.Person.Username, p.Person.DisplayName, p.Person.Status), Access: p.Access, Actions: []apigen.PlannedAction{}}
		for _, a := range p.Actions {
			item.Actions = append(item.Actions, apigen.PlannedAction{Kind: string(a.Kind), Key: a.Key, Removal: a.Removal})
		}
		if p.EmployeeStep != "" {
			item.EmployeeStep = &p.EmployeeStep
		}
		if p.ErrorCode != "" {
			item.ErrorCode = &p.ErrorCode
		}
		out.Items = append(out.Items, item)
	}
	return out, nil
}

func (s *server) RetryAppProvisioning(ctx context.Context, req apigen.RetryAppProvisioningRequestObject) (apigen.RetryAppProvisioningResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	var personID *uuid.UUID
	if req.Body != nil {
		personID = req.Body.PersonId
	}
	n, err := svc.RetryFailed(ctx, actor, req.InstanceId, personID)
	if err != nil {
		return nil, appsError(err)
	}
	return apigen.RetryAppProvisioning202JSONResponse{Queued: n}, nil
}

func (s *server) ConfirmAppRemovalBatch(ctx context.Context, req apigen.ConfirmAppRemovalBatchRequestObject) (apigen.ConfirmAppRemovalBatchResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	hash, err := hex.DecodeString(req.Body.ItemsHash)
	if err != nil || len(hash) != 32 {
		return nil, problemErr(http.StatusBadRequest, "itemsHash must be 64 hex digits.")
	}
	if err := svc.ConfirmRemovals(ctx, actor, req.InstanceId, req.BatchId, hash); err != nil {
		return nil, appsError(err)
	}
	return apigen.ConfirmAppRemovalBatch202Response{}, nil
}

func (s *server) GetPersonApps(ctx context.Context, req apigen.GetPersonAppsRequestObject) (apigen.GetPersonAppsResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	if _, err := a.Identity().GetPerson(ctx, req.PersonId); errors.Is(err, identity.ErrNotFound) {
		return nil, problemErr(http.StatusNotFound, "")
	} else if err != nil {
		return nil, err
	}
	pa, err := svc.ForPerson(ctx, req.PersonId)
	if err != nil {
		return nil, err
	}
	out := apigen.GetPersonApps200JSONResponse{Results: make([]apigen.PersonAppResult, 0, len(pa.Results)), Links: []apigen.AccountLinkView{}}
	for _, r := range pa.Results {
		out.Results = append(out.Results, apigen.PersonAppResult{
			InstanceId: r.InstanceID, InstanceKey: r.InstanceKey, InstanceDisplayName: r.InstanceDisplayName, Provider: r.InstanceProvider,
			InstanceState: apigen.AppInstanceState(r.InstanceState), Status: apigen.ProvisioningStatus(r.Status), Access: r.Access,
			EmployeeStep: r.EmployeeStep, ErrorCode: r.ErrorCode, UpdatedAt: r.UpdatedAt,
		})
	}
	for _, l := range pa.Links {
		out.Links = append(out.Links, verifiedLink(l))
	}
	for _, p := range pa.Pending {
		out.Links = append(out.Links, pendingLink(p))
	}
	return out, nil
}

func pendingLink(p store.ListPersonPendingAccountLinksWithConflictRow) apigen.AccountLinkView {
	org, conflict := p.OrganizationID, p.Conflict
	return apigen.AccountLinkView{
		Provider: p.Provider, Login: p.Login, ExternalId: itoa64(p.ExternalID), State: apigen.AccountLinkViewStatePending,
		OrganizationId: &org, Conflict: &conflict,
	}
}

func verifiedLink(l store.AccountLink) apigen.AccountLinkView {
	id, realm, at := l.ID, l.Realm, l.VerifiedAt
	method := apigen.AccountLinkViewMethod(l.Method)
	v := apigen.AccountLinkView{
		Id: &id, Provider: l.Provider, Realm: &realm, Login: l.Login, ExternalId: l.ExternalID, State: apigen.AccountLinkViewStateVerified,
		Method: &method, InstanceId: l.InstanceID, VerifiedAt: &at,
	}
	if l.Evidence != nil {
		e := apigen.AccountLinkViewEvidence(*l.Evidence)
		v.Evidence = &e
	}
	return v
}

func (s *server) ListMyApps(ctx context.Context, _ apigen.ListMyAppsRequestObject) (apigen.ListMyAppsResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := svc.MyApps(ctx, personID)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.MyApp, 0, len(rows))
	for _, r := range rows {
		item := apigen.MyApp{
			InstanceId: r.Instance.ID, Key: r.Instance.Key, DisplayName: r.Instance.DisplayName, Provider: r.Instance.Provider,
			Capabilities: []string{}, HomeUrl: r.Instance.BaseUrl, Status: apigen.ProvisioningStatus(r.Status),
		}
		if a := r.Instance.Adapter; a != nil {
			for _, c := range a.Manifest.Capabilities {
				item.Capabilities = append(item.Capabilities, string(c))
			}
			if r.Step != "" {
				step := r.Step
				item.EmployeeStep = &step
				for _, es := range a.Manifest.EmployeeSteps {
					if es.ID == step {
						path := es.ActionPath
						item.ActionPath = &path
					}
				}
			}
		}
		items = append(items, item)
	}
	return apigen.ListMyApps200JSONResponse{Items: items}, nil
}

// OpenAppInstance returns a deep link into an app instance for the caller.
func (s *server) OpenAppInstance(ctx context.Context, req apigen.OpenAppInstanceRequestObject) (apigen.OpenAppInstanceResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	u, err := svc.OpenURL(ctx, personID, req.InstanceId, req.Params.To)
	if err != nil {
		return nil, appsError(err)
	}
	return apigen.OpenAppInstance200JSONResponse{Url: u.String()}, nil
}

// ReceiveWebhook takes provider webhooks. The signature over the exact raw
// body is the authentication; nothing is decoded before it is verified.
func (s *server) ReceiveWebhook(ctx context.Context, req apigen.ReceiveWebhookRequestObject) (apigen.ReceiveWebhookResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, apps.MaxWebhookBody+1))
	if err != nil {
		return nil, problemErr(http.StatusBadRequest, "")
	}
	if len(body) > apps.MaxWebhookBody {
		return nil, problemErr(http.StatusRequestEntityTooLarge, "")
	}
	_, r := httpFrom(ctx)
	receipt, err := svc.ReceiveWebhook(ctx, req.InstanceId, r.Header, body)
	switch {
	case errors.Is(err, apps.ErrWebhookUnknown):
		return nil, problemErr(http.StatusNotFound, "")
	case errors.Is(err, apps.ErrWebhookSignature):
		return nil, problemErr(http.StatusUnauthorized, "")
	case errors.Is(err, apps.ErrWebhookMalformed):
		return nil, problemErr(http.StatusBadRequest, "")
	case err != nil:
		return nil, err
	case receipt.Duplicate:
		return apigen.ReceiveWebhook200Response{}, nil
	}
	return apigen.ReceiveWebhook202Response{}, nil
}

func itoa64(n int64) string {
	if n >= 0 && n < 1<<31 {
		return itoa(int(n))
	}
	b, _ := json.Marshal(n)
	return string(b)
}
