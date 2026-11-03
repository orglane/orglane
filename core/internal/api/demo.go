package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/identity"
)

// The operations of demo mode. Their routes exist only when Options.Demo is
// set (see routeGuard), so these handlers never run outside demo mode; the
// nil checks are a second line of defense.

var errNotDemo = problemErr(http.StatusNotFound, "")

func (s *server) GetDemo(context.Context, apigen.GetDemoRequestObject) (apigen.GetDemoResponseObject, error) {
	d := s.opts.Demo
	if d == nil {
		return nil, errNotDemo
	}
	people := make([]apigen.DemoPerson, 0, len(d.People))
	for _, p := range d.People {
		dp := apigen.DemoPerson{Username: p.Username, DisplayName: p.DisplayName, Role: p.Role}
		if p.Title != "" {
			title := p.Title
			dp.Title = &title
		}
		people = append(people, dp)
	}
	return apigen.GetDemo200JSONResponse{
		Dataset:         d.Name,
		Company:         d.Company.Name,
		DefaultUsername: d.DefaultUsername,
		People:          people,
	}, nil
}

func (s *server) SignInAsDemoPerson(ctx context.Context, req apigen.SignInAsDemoPersonRequestObject) (apigen.SignInAsDemoPersonResponseObject, error) {
	d := s.opts.Demo
	if d == nil {
		return nil, errNotDemo
	}
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	w, r := httpFrom(ctx)
	if err := a.AllowSignIn(ctx, r, identity.AuthDemo); err != nil {
		return nil, signInError(err)
	}
	username := d.DefaultUsername
	if req.Body != nil && req.Body.Username != nil {
		username = *req.Body.Username
	}
	// Only people of the dataset, matched by username and work email, so a
	// person added to a demo instance later is never signed in without a
	// credential.
	want, ok := d.Person(username)
	if !ok {
		return nil, problemErr(http.StatusNotFound, "There is no demo person with this username.")
	}
	person, err := a.Identity().GetPersonByUsername(ctx, want.Username)
	if errors.Is(err, identity.ErrNotFound) {
		return nil, problemErr(http.StatusNotFound, "There is no demo person with this username.")
	} else if err != nil {
		return nil, err
	}
	if !strings.EqualFold(person.WorkEmail, want.WorkEmail) {
		return nil, problemErr(http.StatusNotFound, "There is no demo person with this username.")
	}
	if identity.Status(person.Status) != identity.StatusActive {
		return nil, problemErr(http.StatusForbidden, "This demo person is not active.")
	}
	result, err := s.signIn(ctx, a, w, r, person, identity.AuthDemo)
	if err != nil {
		return nil, err
	}
	return apigen.SignInAsDemoPerson200JSONResponse(result), nil
}
