package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/profile"
)

// Profile fields (ADR 0015). Handlers translate between the contract and
// package profile, which applies every visibility and edit rule: nothing is
// filtered here.

func (s *server) profiles() (*profile.Service, error) {
	if s.opts.Profile == nil {
		return nil, errors.New("api: profile fields are not configured")
	}
	return s.opts.Profile, nil
}

// viewer returns the caller as a profile viewer and the audit actor.
func (s *server) viewer(ctx context.Context) (profile.Viewer, audit.Actor, error) {
	a, err := s.auth()
	if err != nil {
		return profile.Viewer{}, audit.Actor{}, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return profile.Viewer{}, audit.Actor{}, err
	}
	p := authz.PrincipalFrom(ctx)
	_, r := httpFrom(ctx)
	return profile.Viewer{
		PersonID:      personID,
		ManagePeople:  p.Has(authz.PeopleManage),
		ReadSensitive: p.Has(authz.PeopleSensitiveRead),
	}, a.Actor(r, personID), nil
}

func (s *server) GetPersonProfile(ctx context.Context, req apigen.GetPersonProfileRequestObject) (apigen.GetPersonProfileResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	viewer, actor, err := s.viewer(ctx)
	if err != nil {
		return nil, err
	}
	entries, err := svc.Profile(ctx, actor, viewer, req.PersonId)
	if err != nil {
		return nil, profileError(err)
	}
	return apigen.GetPersonProfile200JSONResponse(personProfile(req.PersonId, entries)), nil
}

func (s *server) UpdatePersonProfile(ctx context.Context, req apigen.UpdatePersonProfileRequestObject) (apigen.UpdatePersonProfileResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	viewer, actor, err := s.viewer(ctx)
	if err != nil {
		return nil, err
	}
	if len(req.Body.Values) > 200 {
		return nil, problemErr(http.StatusBadRequest, "At most 200 values can change at once.")
	}
	changes := make([]profile.Change, 0, len(req.Body.Values))
	for _, v := range req.Body.Values {
		changes = append(changes, profile.Change{Key: v.Key, Value: v.Value, Hidden: v.Hidden})
	}
	entries, err := svc.UpdateProfile(ctx, actor, viewer, req.PersonId, changes)
	if err != nil {
		return nil, profileError(err)
	}
	return apigen.UpdatePersonProfile200JSONResponse(personProfile(req.PersonId, entries)), nil
}

func (s *server) ListProfileFields(ctx context.Context, req apigen.ListProfileFieldsRequestObject) (apigen.ListProfileFieldsResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	fields, err := svc.Fields(ctx, req.Params.IncludeArchived != nil && *req.Params.IncludeArchived)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.ProfileField, 0, len(fields))
	for _, f := range fields {
		items = append(items, profileField(f))
	}
	return apigen.ListProfileFields200JSONResponse{Items: items}, nil
}

func (s *server) CreateProfileField(ctx context.Context, req apigen.CreateProfileFieldRequestObject) (apigen.CreateProfileFieldResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	_, actor, err := s.viewer(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := profile.NewField{
		Key:                  deref(b.Key),
		Label:                b.Label,
		Type:                 profile.Type(b.Type),
		HandleKind:           profile.HandleKind(derefEnum(b.HandleKind)),
		Options:              options(b.Options),
		Section:              profile.Section(derefEnum(b.Section)),
		Visibility:           profile.Visibility(derefEnum(b.Visibility)),
		RequiredAtOnboarding: deref(b.RequiredAtOnboarding),
		EditableBy:           profile.Editor(derefEnum(b.EditableBy)),
		UserCanHide:          deref(b.UserCanHide),
		Sensitive:            deref(b.Sensitive),
	}
	f, err := svc.CreateField(ctx, actor, in)
	if err != nil {
		return nil, profileError(err)
	}
	return apigen.CreateProfileField201JSONResponse(profileField(f)), nil
}

func (s *server) UpdateProfileField(ctx context.Context, req apigen.UpdateProfileFieldRequestObject) (apigen.UpdateProfileFieldResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	_, actor, err := s.viewer(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := profile.FieldUpdate{
		Label:                b.Label,
		Section:              enumPtr[profile.Section](b.Section),
		Visibility:           enumPtr[profile.Visibility](b.Visibility),
		RequiredAtOnboarding: b.RequiredAtOnboarding,
		EditableBy:           enumPtr[profile.Editor](b.EditableBy),
		UserCanHide:          b.UserCanHide,
		Sensitive:            b.Sensitive,
		Position:             b.Position,
		Archived:             b.Archived,
	}
	if b.Options != nil {
		opts := options(b.Options)
		in.Options = &opts
	}
	f, err := svc.UpdateField(ctx, actor, req.FieldId, in)
	if err != nil {
		return nil, profileError(err)
	}
	return apigen.UpdateProfileField200JSONResponse(profileField(f)), nil
}

func (s *server) ListProfileFieldLibrary(ctx context.Context, _ apigen.ListProfileFieldLibraryRequestObject) (apigen.ListProfileFieldLibraryResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	fields, err := svc.Fields(ctx, true)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]profile.Field, len(fields))
	for _, f := range fields {
		if f.Library {
			byKey[f.Key] = f
		}
	}
	entries := profile.Library()
	items := make([]apigen.ProfileFieldLibraryEntry, 0, len(entries))
	for _, e := range entries {
		item := apigen.ProfileFieldLibraryEntry{
			Key:         e.Key,
			Label:       e.Label,
			Type:        apigen.ProfileFieldType(e.Type),
			HandleKind:  handleKind(e.HandleKind),
			Options:     fieldOptions(e.Options),
			Section:     apigen.ProfileFieldSection(e.Section),
			Visibility:  apigen.ProfileFieldVisibility(e.Visibility),
			EditableBy:  apigen.ProfileFieldEditor(e.EditableBy),
			UserCanHide: e.UserCanHide,
			Sensitive:   e.Sensitive,
		}
		if f, ok := byKey[e.Key]; ok {
			pf := profileField(f)
			item.Field = &pf
		}
		items = append(items, item)
	}
	return apigen.ListProfileFieldLibrary200JSONResponse{Items: items}, nil
}

func (s *server) EnableLibraryProfileField(ctx context.Context, req apigen.EnableLibraryProfileFieldRequestObject) (apigen.EnableLibraryProfileFieldResponseObject, error) {
	svc, err := s.profiles()
	if err != nil {
		return nil, err
	}
	_, actor, err := s.viewer(ctx)
	if err != nil {
		return nil, err
	}
	f, created, err := svc.EnableLibraryField(ctx, actor, req.LibraryKey)
	if err != nil {
		return nil, profileError(err)
	}
	if created {
		return apigen.EnableLibraryProfileField201JSONResponse(profileField(f)), nil
	}
	return apigen.EnableLibraryProfileField200JSONResponse(profileField(f)), nil
}

// profileError maps profile errors to responses.
func profileError(err error) error {
	switch {
	case errors.Is(err, profile.ErrNotFound):
		return problemErr(http.StatusNotFound, "")
	case errors.Is(err, profile.ErrInvalid):
		return problemErr(http.StatusBadRequest, err.Error())
	case errors.Is(err, profile.ErrForbidden):
		return problemErr(http.StatusForbidden, err.Error())
	case errors.Is(err, profile.ErrKeyTaken), errors.Is(err, profile.ErrOptionInUse):
		return problemErr(http.StatusConflict, err.Error())
	default:
		return err
	}
}

func personProfile(personID uuid.UUID, entries []profile.Entry) apigen.PersonProfile {
	out := apigen.PersonProfile{PersonId: personID, Fields: make([]apigen.ProfileFieldValue, 0, len(entries))}
	for _, e := range entries {
		v := apigen.ProfileFieldValue{
			Field:      profileField(e.Field),
			Hidden:     e.Hidden,
			YearHidden: e.YearHidden,
			Editable:   e.Editable,
			Hideable:   e.Hideable,
		}
		if e.Value != "" {
			value := e.Value
			v.Value = &value
		}
		out.Fields = append(out.Fields, v)
	}
	return out
}

func profileField(f profile.Field) apigen.ProfileField {
	return apigen.ProfileField{
		Id:                   f.ID,
		Key:                  f.Key,
		Library:              f.Library,
		Label:                f.Label,
		Type:                 apigen.ProfileFieldType(f.Type),
		HandleKind:           handleKind(f.HandleKind),
		Options:              fieldOptions(f.Options),
		Section:              apigen.ProfileFieldSection(f.Section),
		Visibility:           apigen.ProfileFieldVisibility(f.Visibility),
		RequiredAtOnboarding: f.RequiredAtOnboarding,
		EditableBy:           apigen.ProfileFieldEditor(f.EditableBy),
		UserCanHide:          f.UserCanHide,
		Sensitive:            f.Sensitive,
		Position:             f.Position,
		Archived:             f.Archived,
	}
}

func handleKind(k profile.HandleKind) *apigen.ProfileFieldHandleKind {
	if k == "" {
		return nil
	}
	v := apigen.ProfileFieldHandleKind(k)
	return &v
}

func fieldOptions(in []profile.Option) []apigen.ProfileFieldOption {
	out := make([]apigen.ProfileFieldOption, 0, len(in))
	for _, o := range in {
		out = append(out, apigen.ProfileFieldOption{Key: o.Key, Label: o.Label})
	}
	return out
}

func options(in *[]apigen.NewProfileFieldOption) []profile.Option {
	if in == nil {
		return nil
	}
	out := make([]profile.Option, 0, len(*in))
	for _, o := range *in {
		out = append(out, profile.Option{Key: deref(o.Key), Label: o.Label})
	}
	return out
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

func derefEnum[T ~string](p *T) string {
	if p == nil {
		return ""
	}
	return string(*p)
}

func enumPtr[D ~string, S ~string](p *S) *D {
	if p == nil {
		return nil
	}
	v := D(*p)
	return &v
}
