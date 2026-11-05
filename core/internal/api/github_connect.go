package api

import (
	"context"
	"errors"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/apps"
	"github.com/orglane/orglane/core/internal/authz"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

func connectSession(ctx context.Context) (uuid.UUID, uuid.UUID, error) {
	person, err := callerID(ctx)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	session, err := uuid.Parse(authz.PrincipalFrom(ctx).SessionID)
	if err != nil {
		return uuid.Nil, uuid.Nil, problemErr(http.StatusForbidden, "Connect GitHub using a browser session.")
	}
	return person, session, nil
}
func connectResponse(r apps.ConnectReply) apigen.GitHubConnectStep {
	out := apigen.GitHubConnectStep{InstanceId: r.InstanceID, Done: r.Done}
	if r.Step != nil {
		step := r.Step
		u := step.URL.String()
		method := apigen.GitHubConnectStepMethod(step.Method)
		out.Name = &step.Name
		out.Method = &method
		out.Url = &u
		if len(step.Form) > 0 {
			out.Form = &step.Form
		}
	}
	return out
}
func connectError(err error) error {
	var pe *provider.Error
	if errors.As(err, &pe) && pe.Code == githubprovider.CodeRateLimited {
		return &apiError{status: http.StatusTooManyRequests, code: pe.Code, detail: "GitHub's rate limit was reached. Try again later.", retryAfter: seconds(pe.RetryAfter.Seconds())}
	}
	if errors.As(err, &pe) {
		return codedProblem(http.StatusBadRequest, pe.Code, "GitHub could not verify this connection. Review the organization and App settings, then try again.")
	}
	// Only a known provider HTTP failure is a safe client-facing retry error.
	var upstream *githubprovider.APIError
	if errors.As(err, &upstream) {
		return codedProblem(http.StatusBadRequest, "github_unavailable", "GitHub could not verify this connection. Try again.")
	}
	return appsError(err)
}
func (s *server) BeginGitHubConnect(ctx context.Context, req apigen.BeginGitHubConnectRequestObject) (apigen.BeginGitHubConnectResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	person, session, err := connectSession(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	manual := url.Values{}
	if b.Manual != nil {
		m := b.Manual
		manual.Set("app_id", m.AppId)
		manual.Set("client_id", m.ClientId)
		manual.Set("private_key", valueString(m.PrivateKey))
		manual.Set("client_secret", valueString(m.ClientSecret))
		manual.Set("webhook_secret", valueString(m.WebhookSecret))
	}
	reply, err := svc.BeginGitHubConnect(ctx, actor, person, session, b.Organization, b.Key, b.InstanceId, manual)
	if err != nil {
		return nil, connectError(err)
	}
	return apigen.BeginGitHubConnect200JSONResponse(connectResponse(reply)), nil
}
func (s *server) CompleteGitHubConnect(ctx context.Context, req apigen.CompleteGitHubConnectRequestObject) (apigen.CompleteGitHubConnectResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	person, session, err := connectSession(ctx)
	if err != nil {
		return nil, err
	}
	params := url.Values{}
	if req.Body.Code != nil {
		params.Set("code", *req.Body.Code)
	}
	if req.Body.InstallationId != nil {
		params.Set("installation_id", *req.Body.InstallationId)
	}
	reply, err := svc.CompleteGitHubConnect(ctx, actor, person, session, req.Body.State, params)
	if err != nil {
		return nil, connectError(err)
	}
	return apigen.CompleteGitHubConnect200JSONResponse(connectResponse(reply)), nil
}
func (s *server) UploadAppCredential(ctx context.Context, req apigen.UploadAppCredentialRequestObject) (apigen.UploadAppCredentialResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	value := []byte(valueString(req.Body.Value))
	defer clear(value)
	if err = svc.UploadCredential(ctx, actor, req.InstanceId, req.CredentialName, value); err != nil {
		return nil, appsError(err)
	}
	return apigen.UploadAppCredential202Response{}, nil
}

func valueString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
