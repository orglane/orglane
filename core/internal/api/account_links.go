package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/apps"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

// Account links of the caller (docs/specs/providers.md "Account links").

func (s *server) ListMyAccountLinks(ctx context.Context, _ apigen.ListMyAccountLinksRequestObject) (apigen.ListMyAccountLinksResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	person, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	links, err := svc.MyAccountLinks(ctx, person)
	if err != nil {
		return nil, err
	}
	out := apigen.ListMyAccountLinks200JSONResponse{Links: []apigen.AccountLinkView{}, Linkable: []apigen.LinkableProvider{}}
	for _, l := range links.Verified {
		out.Links = append(out.Links, verifiedLink(l))
	}
	for _, p := range links.Pending {
		out.Links = append(out.Links, pendingLink(p))
	}
	for _, inst := range links.Linkable {
		out.Linkable = append(out.Linkable, apigen.LinkableProvider{DisplayName: inst.DisplayName, InstanceId: inst.ID, Provider: inst.Provider})
	}
	return out, nil
}

func (s *server) StartAccountLink(ctx context.Context, req apigen.StartAccountLinkRequestObject) (apigen.StartAccountLinkResponseObject, error) {
	svc, err := s.appsService()
	if err != nil {
		return nil, err
	}
	person, session, err := linkSession(ctx)
	if err != nil {
		return nil, err
	}
	var instanceID *uuid.UUID
	if req.Body != nil {
		instanceID = req.Body.InstanceId
	}
	u, err := svc.StartAccountLink(ctx, person, session, req.Provider, instanceID)
	if err != nil {
		return nil, linkError(err)
	}
	return apigen.StartAccountLink200JSONResponse{Url: u.String()}, nil
}

func (s *server) CompleteAccountLink(ctx context.Context, req apigen.CompleteAccountLinkRequestObject) (apigen.CompleteAccountLinkResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	person, session, err := linkSession(ctx)
	if err != nil {
		return nil, err
	}
	link, err := svc.CompleteAccountLink(ctx, actor, person, session, req.Provider, req.Body.State, req.Body.Code)
	if err != nil {
		return nil, linkError(err)
	}
	return apigen.CompleteAccountLink200JSONResponse(verifiedLink(link)), nil
}

func (s *server) UnlinkMyAccount(ctx context.Context, req apigen.UnlinkMyAccountRequestObject) (apigen.UnlinkMyAccountResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	person, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := svc.RevokeAccountLink(ctx, actor, person, req.LinkId, apps.RevokedBySelf); err != nil {
		return nil, appsError(err)
	}
	return apigen.UnlinkMyAccount204Response{}, nil
}

func (s *server) RevokePersonAccountLink(ctx context.Context, req apigen.RevokePersonAccountLinkRequestObject) (apigen.RevokePersonAccountLinkResponseObject, error) {
	svc, actor, err := s.appsManager(ctx)
	if err != nil {
		return nil, err
	}
	if err := svc.RevokeAccountLink(ctx, actor, req.PersonId, req.LinkId, apps.RevokedByAdmin); err != nil {
		return nil, appsError(err)
	}
	return apigen.RevokePersonAccountLink204Response{}, nil
}

// linkSession is the caller and their browser session: link states are
// bound to both, so verification needs a session, not a bearer token.
func linkSession(ctx context.Context) (personID, sessionID uuid.UUID, err error) {
	personID, sessionID, err = connectSession(ctx)
	if err != nil {
		return personID, sessionID, problemErr(http.StatusForbidden, "Link accounts from a browser session.")
	}
	return personID, sessionID, nil
}

// linkError maps a provider's refusal to its safe code (never upstream
// bodies or diagnostics); framework errors keep theirs.
func linkError(err error) error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		if pe.Code == githubprovider.CodeRateLimited {
			return &apiError{status: http.StatusTooManyRequests, code: pe.Code, detail: "The provider's rate limit was reached. Try again later.", retryAfter: seconds(pe.RetryAfter.Seconds())}
		}
		return codedProblem(http.StatusBadRequest, pe.Code, "The provider did not confirm your account. Try again.")
	}
	var upstream *githubprovider.APIError
	if errors.As(err, &upstream) {
		return codedProblem(http.StatusBadRequest, "provider_unavailable", "The provider could not be reached. Try again.")
	}
	return appsError(err)
}
