package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/mail"
	"github.com/orglane/orglane/core/internal/store"
)

// usernamePlaceholder stands for the username in the work email pattern.
const usernamePlaceholder = "{username}"

func (s *server) ListInvitations(ctx context.Context, _ apigen.ListInvitationsRequestObject) (apigen.ListInvitationsResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	rows, err := a.Identity().Invitations(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.Invitation, 0, len(rows))
	now := time.Now()
	for _, row := range rows {
		items = append(items, invitation(row, now))
	}
	return apigen.ListInvitations200JSONResponse{Items: items}, nil
}

func (s *server) GetInvitationOptions(ctx context.Context, _ apigen.GetInvitationOptionsRequestObject) (apigen.GetInvitationOptionsResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	delivery, err := a.Identity().InvitationDelivery(ctx)
	if err != nil {
		return nil, err
	}
	roles := make([]string, 0, 3)
	for _, role := range authz.BuiltinRoles() {
		allowed := true
		for _, permission := range role.Permissions {
			if !authz.PrincipalFrom(ctx).Has(permission) {
				allowed = false
				break
			}
		}
		if allowed {
			roles = append(roles, string(role.Key))
		}
	}
	return apigen.GetInvitationOptions200JSONResponse{
		Roles:            roles,
		Delivery:         apigen.InvitationDelivery(delivery),
		WorkEmailPattern: s.workEmail(usernamePlaceholder),
		LifetimeHours:    int(a.Identity().InvitationLifetime().Hours()),
	}, nil
}

func (s *server) CreateInvitation(ctx context.Context, req apigen.CreateInvitationRequestObject) (apigen.CreateInvitationResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	role := authz.RoleKey(strings.TrimSpace(b.Role))
	if err := s.canGrant(ctx, a.Identity(), role); err != nil {
		return nil, err
	}
	in := identity.NewInvitation{
		DisplayName:  b.DisplayName,
		ContactEmail: b.ContactEmail,
		Username:     b.Username,
		Role:         role,
	}
	if b.TeamIds != nil {
		in.TeamIDs = *b.TeamIds
	}
	if b.Title != nil {
		in.Title = *b.Title
	}
	if b.WorkEmail != nil && strings.TrimSpace(*b.WorkEmail) != "" {
		in.WorkEmail = *b.WorkEmail
	} else {
		in.WorkEmail = s.workEmail(strings.ToLower(strings.TrimSpace(b.Username)))
	}
	_, r := httpFrom(ctx)
	in.Locale = r.Header.Get("Accept-Language")
	issued, err := a.Identity().Invite(ctx, a.Actor(r, personID), in)
	if err != nil {
		return nil, invitationError(err)
	}
	out, err := s.issued(ctx, a.Identity(), issued)
	if err != nil {
		return nil, err
	}
	return apigen.CreateInvitation201JSONResponse(out), nil
}

func (s *server) ResendInvitation(ctx context.Context, req apigen.ResendInvitationRequestObject) (apigen.ResendInvitationResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.canManageInvitation(ctx, a.Identity(), req.InvitationId); err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	issued, err := a.Identity().ResendInvitation(ctx, a.Actor(r, personID), req.InvitationId)
	if err != nil {
		return nil, invitationError(err)
	}
	out, err := s.issued(ctx, a.Identity(), issued)
	if err != nil {
		return nil, err
	}
	return apigen.ResendInvitation200JSONResponse(out), nil
}

func (s *server) RevokeInvitation(ctx context.Context, req apigen.RevokeInvitationRequestObject) (apigen.RevokeInvitationResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.canManageInvitation(ctx, a.Identity(), req.InvitationId); err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	if err := a.Identity().RevokeInvitation(ctx, a.Actor(r, personID), req.InvitationId); err != nil {
		return nil, invitationError(err)
	}
	return apigen.RevokeInvitation204Response{}, nil
}

func (s *server) PreviewInvitation(ctx context.Context, req apigen.PreviewInvitationRequestObject) (apigen.PreviewInvitationResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthPassword); err != nil {
		return nil, signInError(err)
	}
	preview, err := a.Identity().PreviewInvitation(ctx, strings.TrimSpace(req.Body.Token))
	if err != nil {
		return nil, invitationError(err)
	}
	return apigen.PreviewInvitation200JSONResponse{
		DisplayName: preview.Person.DisplayName,
		Username:    preview.Person.Username,
		WorkEmail:   preview.Person.WorkEmail,
		ExpiresAt:   preview.ExpiresAt,
	}, nil
}

func (s *server) AcceptInvitation(ctx context.Context, req apigen.AcceptInvitationRequestObject) (apigen.AcceptInvitationResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	w, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthPassword); err != nil {
		return nil, signInError(err)
	}
	person, err := a.Identity().AcceptInvitation(ctx, a.Actor(r, uuid.Nil), strings.TrimSpace(req.Body.Token), req.Body.Password)
	if err != nil {
		return nil, invitationError(err)
	}
	result, err := s.signIn(ctx, a, w, r, person, identity.AuthPassword)
	if err != nil {
		return nil, err
	}
	return apigen.AcceptInvitation200JSONResponse(result), nil
}

// canGrant checks that the caller holds every permission of the role, so
// inviting cannot hand out more than the inviter has (an Admin cannot
// invite an Owner).
func (s *server) canGrant(ctx context.Context, svc *identity.Service, role authz.RoleKey) error {
	if role == "" {
		return problemErr(http.StatusBadRequest, "role: required")
	}
	perms, err := svc.RolePermissions(ctx, role)
	if errors.Is(err, identity.ErrNotFound) {
		return problemErr(http.StatusBadRequest, "role: unknown role")
	} else if err != nil {
		return err
	}
	principal := authz.PrincipalFrom(ctx)
	for _, p := range perms {
		if !principal.Has(p) {
			return problemErr(http.StatusForbidden, "Granting this role requires the "+string(p)+" permission.")
		}
	}
	return nil
}

// Resending can reveal a link in link mode. Apply the same grant check as
// creation, so an admin cannot take over an outstanding owner invitation.
func (s *server) canManageInvitation(ctx context.Context, svc *identity.Service, id uuid.UUID) error {
	inv, err := svc.InvitationDetails(ctx, id)
	if err != nil {
		return invitationError(err)
	}
	for _, role := range inv.RoleKeys {
		if err := s.canGrant(ctx, svc, authz.RoleKey(role)); err != nil {
			return err
		}
	}
	return nil
}

func (s *server) workEmail(username string) string {
	if s.opts.WorkEmail == nil {
		return username + "@" + s.opts.BaseDomain
	}
	return s.opts.WorkEmail(username)
}

func (s *server) issued(ctx context.Context, svc *identity.Service, issued identity.IssuedInvitation) (apigen.IssuedInvitation, error) {
	row, err := svc.InvitationDetails(ctx, issued.Invitation.ID)
	if err != nil {
		return apigen.IssuedInvitation{}, err
	}
	out := apigen.IssuedInvitation{Invitation: invitation(row, time.Now())}
	if issued.Link != "" {
		out.Link = &issued.Link
	}
	return out, nil
}

func invitation(row store.ListInvitationsRow, now time.Time) apigen.Invitation {
	inv := apigen.Invitation{
		Id:          row.ID,
		PersonId:    row.PersonID,
		DisplayName: row.DisplayName,
		Username:    row.Username,
		WorkEmail:   row.WorkEmail,
		Roles:       row.RoleKeys,
		InvitedBy:   row.InvitedBy,
		CreatedAt:   row.CreatedAt,
		IssuedAt:    row.IssuedAt,
		ExpiresAt:   row.ExpiresAt,
		Delivery:    apigen.InvitationDelivery(identity.DeliveryLink),
	}
	if row.ContactEmail != nil {
		inv.ContactEmail = *row.ContactEmail
	}
	switch {
	case row.RevokedAt != nil:
		inv.Status = apigen.InvitationStatus("revoked")
	case !now.Before(row.ExpiresAt):
		inv.Status = apigen.InvitationStatus("expired")
	default:
		inv.Status = apigen.InvitationStatus("pending")
	}
	if row.EmailStatus != nil {
		inv.Delivery = apigen.InvitationDelivery(identity.DeliveryEmail)
		d := apigen.EmailDelivery{
			Status:    apigen.EmailDeliveryStatus(*row.EmailStatus),
			LastError: row.EmailLastError,
			SentAt:    row.EmailSentAt,
		}
		if row.EmailAttempts != nil {
			d.Attempts = int(*row.EmailAttempts)
		}
		if row.EmailMaxAttempts != nil {
			d.MaxAttempts = int(*row.EmailMaxAttempts)
		}
		if mail.Status(d.Status) == mail.StatusQueued && d.Attempts > 0 {
			d.Status = apigen.EmailDeliveryStatus("retrying")
		}
		inv.Email = &d
	}
	return inv
}

// invitationError maps invitation failures to responses.
func invitationError(err error) error {
	switch {
	case errors.Is(err, identity.ErrInvitationInvalid):
		return problemErr(http.StatusNotFound, "This invitation link is not valid. It may have been revoked or replaced by a newer one.")
	case errors.Is(err, identity.ErrInvitationExpired):
		return problemErr(http.StatusGone, "This invitation link has expired. Ask an admin to resend it.")
	case errors.Is(err, identity.ErrInvitationUsed):
		return problemErr(http.StatusConflict, "This invitation was already accepted.")
	case errors.Is(err, identity.ErrTeamArchived):
		return teamError(err)
	case errors.Is(err, identity.ErrNotFound):
		return problemErr(http.StatusNotFound, "")
	case errors.Is(err, mail.ErrInvalidAddress):
		return problemErr(http.StatusBadRequest, "contact_email: must be a bare address such as name@example.com")
	default:
		return inputError(err)
	}
}
