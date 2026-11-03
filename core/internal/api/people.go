package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
)

// avatarHeaders make an avatar inert even when opened directly: no content
// sniffing, no scripts, styles from outside or subresources (sandboxed), and
// no embedding by other sites.
var avatarHeaders = map[string]string{
	"X-Content-Type-Options":       "nosniff",
	"Content-Security-Policy":      "default-src 'none'; style-src 'unsafe-inline'; sandbox",
	"Cross-Origin-Resource-Policy": "same-origin",
	"Cache-Control":                "private, max-age=300",
}

func (s *server) ListPeople(ctx context.Context, req apigen.ListPeopleRequestObject) (apigen.ListPeopleResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	if req.Params.Limit != nil && (*req.Params.Limit < 1 || *req.Params.Limit > 100) {
		return nil, problemErr(http.StatusBadRequest, "Limit must be between 1 and 100.")
	}
	page, err := a.Identity().DirectoryPage(ctx, identity.DirectoryOptions{Search: deref(req.Params.Q), TeamID: req.Params.Team, Role: deref(req.Params.Role), Limit: deref(req.Params.Limit), Cursor: req.Params.Cursor, UpdatedSince: req.Params.UpdatedSince})
	if err != nil {
		return nil, personError(err)
	}
	body := apigen.PersonList{Items: make([]apigen.Person, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, row := range page.Items {
		body.Items = append(body.Items, publicPerson(row))
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	etag := fmt.Sprintf(`"%x"`, sha256.Sum256(raw))
	w, _ := httpFrom(ctx)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Vary", "Cookie, Authorization")
	if matchETag(deref(req.Params.IfNoneMatch), etag) {
		return apigen.ListPeople304Response{Headers: apigen.ListPeople304ResponseHeaders{ETag: &etag}}, nil
	}
	return apigen.ListPeople200JSONResponse{Body: body, Headers: apigen.ListPeople200ResponseHeaders{ETag: &etag}}, nil
}

func matchETag(header, etag string) bool {
	for _, value := range strings.Split(header, ",") {
		value = strings.TrimSpace(value)
		if value == "*" || strings.TrimPrefix(value, "W/") == etag {
			return true
		}
	}
	return false
}

func publicPerson(row identity.DirectoryPerson) apigen.Person {
	p := row.Person
	if identity.Status(p.Status) == identity.StatusOffboarded {
		return apigen.Person{Id: p.ID, Status: apigen.PersonStatus("offboarded"), UpdatedAt: p.DirectoryUpdatedAt, Roles: []string{}, Teams: []apigen.DirectoryTeam{}}
	}
	out := apigen.Person{Id: p.ID, Username: p.Username, WorkEmail: p.WorkEmail, DisplayName: p.DisplayName, Title: p.Title, Status: apigen.PersonStatus(p.Status), Roles: row.Roles, UpdatedAt: p.DirectoryUpdatedAt, ManagerId: p.ManagerID, Teams: make([]apigen.DirectoryTeam, 0, len(row.Teams))}
	for _, t := range row.Teams {
		out.Teams = append(out.Teams, apigen.DirectoryTeam{Id: t.ID, Name: t.Name, Slug: t.Slug})
	}
	if row.HasAvatar {
		url := avatarURL(p.ID) + "?v=" + strconv.FormatInt(p.DirectoryUpdatedAt.UnixMicro(), 10)
		out.AvatarUrl = &url
	}
	return out
}

func (s *server) GetPerson(ctx context.Context, req apigen.GetPersonRequestObject) (apigen.GetPersonResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	person, err := a.Identity().DirectoryPerson(ctx, req.PersonId)
	if err != nil {
		return nil, personError(err)
	}
	w, _ := httpFrom(ctx)
	w.Header().Set("Cache-Control", "private, no-cache")
	return apigen.GetPerson200JSONResponse(publicPerson(person)), nil
}

func (s *server) UpdatePerson(ctx context.Context, req apigen.UpdatePersonRequestObject) (apigen.UpdatePersonResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	viewer, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	manage := authz.PrincipalFrom(ctx).Has(authz.PeopleManage)
	b := req.Body
	if !manage && (viewer != req.PersonId || b.Username != nil || b.WorkEmail != nil || b.ManagerId != nil) {
		return nil, problemErr(http.StatusForbidden, "You cannot edit these profile fields.")
	}
	if _, err := a.Identity().DirectoryPerson(ctx, req.PersonId); err != nil {
		return nil, personError(err)
	}
	in := identity.PersonUpdate{Username: b.Username, WorkEmail: b.WorkEmail, DisplayName: b.DisplayName, Title: b.Title}
	if b.ManagerId != nil {
		id := uuid.Nil
		if *b.ManagerId != "" {
			id, err = uuid.Parse(*b.ManagerId)
			if err != nil {
				return nil, problemErr(http.StatusBadRequest, "Manager must be a person ID or empty.")
			}
		}
		in.ManagerID = &id
	}
	if _, err := a.Identity().UpdatePerson(ctx, actor, req.PersonId, in); err != nil {
		return nil, personError(err)
	}
	person, err := a.Identity().DirectoryPerson(ctx, req.PersonId)
	if err != nil {
		return nil, personError(err)
	}
	return apigen.UpdatePerson200JSONResponse(publicPerson(person)), nil
}

func (s *server) UploadPersonAvatar(ctx context.Context, req apigen.UploadPersonAvatarRequestObject) (apigen.UploadPersonAvatarResponseObject, error) {
	a, actor, err := s.manager(ctx)
	if err != nil {
		return nil, err
	}
	caller, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	if caller != req.PersonId && !authz.PrincipalFrom(ctx).Has(authz.PeopleManage) {
		return nil, problemErr(http.StatusForbidden, "You cannot change this avatar.")
	}
	if _, err := a.Identity().DirectoryPerson(ctx, req.PersonId); err != nil {
		return nil, personError(err)
	}
	_, r := httpFrom(ctx)
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || (media != identity.AvatarPNG && media != "image/jpeg" && media != identity.AvatarWebP) {
		return nil, problemErr(http.StatusUnsupportedMediaType, "Upload a PNG, JPEG or WebP image.")
	}
	if r.ContentLength > identity.MaxAvatarUploadBytes {
		return nil, problemErr(http.StatusRequestEntityTooLarge, "Avatar exceeds 5 MiB.")
	}
	// Read the actual request stream: OpenAPI's image/* media range accepts
	// concrete raster Content-Types, while generated prefix checks do not.
	data, err := io.ReadAll(io.LimitReader(r.Body, identity.MaxAvatarUploadBytes+1))
	if err != nil {
		return nil, problemErr(http.StatusBadRequest, "Could not read the avatar.")
	}
	if len(data) > identity.MaxAvatarUploadBytes {
		return nil, problemErr(http.StatusRequestEntityTooLarge, "Avatar exceeds 5 MiB.")
	}
	if err := a.Identity().UploadAvatar(ctx, actor, req.PersonId, media, data); err != nil {
		return nil, personError(err)
	}
	return apigen.UploadPersonAvatar204Response{}, nil
}

func personError(err error) error {
	switch {
	case errors.Is(err, identity.ErrNotFound):
		return problemErr(http.StatusNotFound, "")
	case errors.Is(err, identity.ErrInvalid), errors.Is(err, identity.ErrUnsafeAvatar):
		return problemErr(http.StatusBadRequest, err.Error())
	case errors.Is(err, identity.ErrUsernameTaken), errors.Is(err, identity.ErrEmailTaken):
		return problemErr(http.StatusConflict, err.Error())
	default:
		return err
	}
}

func (s *server) GetPersonAvatar(ctx context.Context, req apigen.GetPersonAvatarRequestObject) (apigen.GetPersonAvatarResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	person, err := a.Identity().GetPerson(ctx, req.PersonId)
	if errors.Is(err, identity.ErrNotFound) || (err == nil && identity.Status(person.Status) == identity.StatusOffboarded) {
		return nil, problemErr(http.StatusNotFound, "")
	} else if err != nil {
		return nil, err
	}
	avatar, err := a.Identity().GetAvatar(ctx, req.PersonId)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, problemErr(http.StatusNotFound, "")
	} else if err != nil {
		return nil, err
	}
	w, _ := httpFrom(ctx)
	for k, v := range avatarHeaders {
		w.Header().Set(k, v)
	}
	body, size := bytes.NewReader(avatar.Data), int64(len(avatar.Data))
	// The content type comes from the stored, allow-listed value; each
	// response type sets it as a constant.
	switch avatar.ContentType {
	case identity.AvatarSVG:
		return apigen.GetPersonAvatar200ImagesvgXmlResponse{Body: body, ContentLength: size}, nil
	case identity.AvatarPNG:
		return apigen.GetPersonAvatar200ImagepngResponse{Body: body, ContentLength: size}, nil
	case identity.AvatarWebP:
		return apigen.GetPersonAvatar200ImagewebpResponse{Body: body, ContentLength: size}, nil
	default:
		return nil, errors.New("api: avatar has an unexpected content type")
	}
}
