package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"

	"github.com/orglane/orglane/core/internal/api/apigen"
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

func (s *server) ListPeople(ctx context.Context, _ apigen.ListPeopleRequestObject) (apigen.ListPeopleResponseObject, error) {
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	rows, err := a.Identity().Directory(ctx)
	if err != nil {
		return nil, err
	}
	items := make([]apigen.Person, 0, len(rows))
	for _, row := range rows {
		p := apigen.Person{
			Id:          row.ID,
			Username:    row.Username,
			WorkEmail:   row.WorkEmail,
			DisplayName: row.DisplayName,
			Title:       row.Title,
			Status:      apigen.PersonStatus(row.Status),
			Roles:       row.RoleKeys,
		}
		if row.HasAvatar {
			url := VersionPrefix + "people/" + row.ID.String() + "/avatar"
			p.AvatarUrl = &url
		}
		items = append(items, p)
	}
	return apigen.ListPeople200JSONResponse{Items: items}, nil
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
