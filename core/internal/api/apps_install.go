package api

import (
	"context"
	"net/http"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/providers/provider"
)

// The install API of managed providers (#61): install, remove or purge,
// reinstall, and the agent's status.

func (s *server) InstallAppInstance(ctx context.Context, req apigen.InstallAppInstanceRequestObject) (apigen.InstallAppInstanceResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	if b == nil {
		return nil, problemErr(http.StatusBadRequest, "A request body is required.")
	}
	in := apps.NewInstance{Provider: provider.ID(b.Provider)}
	if b.Key != nil {
		in.Key = *b.Key
	}
	if b.DisplayName != nil {
		in.DisplayName = *b.DisplayName
	}
	if b.Host != nil && *b.Host != "" {
		in.BaseURL = svc.HostURL(*b.Host)
	}
	if b.Settings != nil {
		in.Settings = *b.Settings
	}
	inst, err := svc.Install(ctx, actor, in)
	if err != nil {
		return nil, appsError(err)
	}
	out, err := s.appInstance(ctx, svc, inst)
	if err != nil {
		return nil, err
	}
	return apigen.InstallAppInstance202JSONResponse(out), nil
}

func (s *server) RemoveAppInstance(ctx context.Context, req apigen.RemoveAppInstanceRequestObject) (apigen.RemoveAppInstanceResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	purge := req.Params.Purge != nil && *req.Params.Purge
	confirm := ""
	if req.Params.Confirm != nil {
		confirm = *req.Params.Confirm
	}
	if _, err := svc.Remove(ctx, actor, req.InstanceId, purge, confirm); err != nil {
		return nil, appsError(err)
	}
	inst, err := svc.Instance(ctx, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	out, err := s.appInstance(ctx, svc, inst)
	if err != nil {
		return nil, err
	}
	return apigen.RemoveAppInstance202JSONResponse(out), nil
}

func (s *server) ReinstallAppInstance(ctx context.Context, req apigen.ReinstallAppInstanceRequestObject) (apigen.ReinstallAppInstanceResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := svc.Reinstall(ctx, actor, req.InstanceId); err != nil {
		return nil, appsError(err)
	}
	inst, err := svc.Instance(ctx, req.InstanceId)
	if err != nil {
		return nil, appsError(err)
	}
	out, err := s.appInstance(ctx, svc, inst)
	if err != nil {
		return nil, err
	}
	return apigen.ReinstallAppInstance202JSONResponse(out), nil
}

func (s *server) GetAppAgent(ctx context.Context, _ apigen.GetAppAgentRequestObject) (apigen.GetAppAgentResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	st, err := svc.AgentStatus(ctx)
	if err != nil {
		return nil, err
	}
	out := apigen.GetAppAgent200JSONResponse{
		Enabled: st.Enabled, Connected: st.Connected,
		MemTotal: int64(st.MemTotal), MemAvailable: int64(st.MemAvailable), //nolint:gosec // host sizes fit
		DiskFree: int64(st.DiskFree), MemoryReserved: int64(st.MemoryReserved), //nolint:gosec // host sizes fit
	}
	if st.Code != "" {
		c := apigen.AppAgentStatusCode(st.Code)
		out.Code = &c
	}
	if st.Version != "" {
		out.Version = &st.Version
	}
	return out, nil
}

// installDefaults is the install part of a managed provider, nil when it
// cannot be installed in this build.
func installDefaults(svc *apps.Service, a *apps.Adapter) *apigen.AppInstallDefaults {
	d, ok := svc.InstallDefaults(a)
	if !ok {
		return nil
	}
	r := d.Package.Manifest.Resources
	return &apigen.AppInstallDefaults{
		Key: d.Key, Host: d.Host, Settings: d.Settings, PackageVersion: d.Package.Manifest.Version,
		MemoryMin: int64(r.Memory.Min), MemoryRecommended: int64(r.Memory.Recommended), DiskMin: int64(r.Disk.Min),
	}
}
