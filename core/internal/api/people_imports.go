package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/peopleimport"
)

func (s *server) importActor(ctx context.Context) (audit.Actor, error) {
	a, err := s.auth()
	if err != nil {
		return audit.Actor{}, err
	}
	id, err := callerID(ctx)
	if err != nil {
		return audit.Actor{}, err
	}
	_, r := httpFrom(ctx)
	return a.Actor(r, id), nil
}
func importError(err error) error {
	switch {
	case errors.Is(err, peopleimport.ErrFile), errors.Is(err, peopleimport.ErrInvalid):
		return problemErr(http.StatusBadRequest, err.Error())
	case errors.Is(err, peopleimport.ErrForbidden):
		return problemErr(http.StatusForbidden, "")
	case errors.Is(err, peopleimport.ErrNotFound):
		return problemErr(http.StatusNotFound, "")
	case errors.Is(err, peopleimport.ErrState):
		return problemErr(http.StatusConflict, err.Error())
	default:
		return err
	}
}
func importResponse(run peopleimport.Run) (apigen.PeopleImport, error) {
	b, err := json.Marshal(run)
	if err != nil {
		return apigen.PeopleImport{}, err
	}
	var out apigen.PeopleImport
	err = json.Unmarshal(b, &out)
	return out, err
}
func (s *server) GetPeopleImportTemplate(_ context.Context, _ apigen.GetPeopleImportTemplateRequestObject) (apigen.GetPeopleImportTemplateResponseObject, error) {
	return apigen.GetPeopleImportTemplate200TextcsvResponse{Body: strings.NewReader(peopleimport.Template())}, nil
}
func (s *server) UploadPeopleImport(ctx context.Context, req apigen.UploadPeopleImportRequestObject) (apigen.UploadPeopleImportResponseObject, error) {
	if s.opts.Imports == nil {
		return nil, errors.New("api: imports are not configured")
	}
	actor, err := s.importActor(ctx)
	if err != nil {
		return nil, err
	}
	// The generated binary request body remains a reader, bounded by the source.
	run, err := s.opts.Imports.Upload(ctx, actor, peopleimport.FileSource{}, io.LimitReader(req.Body, peopleimport.MaxUploadBytes+1))
	if err != nil {
		return nil, importError(err)
	}
	out, err := importResponse(run)
	if err != nil {
		return nil, err
	}
	return apigen.UploadPeopleImport201JSONResponse(out), nil
}
func (s *server) GetPeopleImport(ctx context.Context, req apigen.GetPeopleImportRequestObject) (apigen.GetPeopleImportResponseObject, error) {
	if s.opts.Imports == nil {
		return nil, errors.New("api: imports are not configured")
	}
	actor, err := s.importActor(ctx)
	if err != nil {
		return nil, err
	}
	run, err := s.opts.Imports.Get(ctx, actor, req.RunId)
	if err != nil {
		return nil, importError(err)
	}
	out, err := importResponse(run)
	if err != nil {
		return nil, err
	}
	return apigen.GetPeopleImport200JSONResponse(out), nil
}
func (s *server) PreviewPeopleImport(ctx context.Context, req apigen.PreviewPeopleImportRequestObject) (apigen.PreviewPeopleImportResponseObject, error) {
	if s.opts.Imports == nil {
		return nil, errors.New("api: imports are not configured")
	}
	actor, err := s.importActor(ctx)
	if err != nil {
		return nil, err
	}
	mapping := peopleimport.Mapping{Columns: []peopleimport.Column{}, CreateTeams: req.Body.CreateTeams, SendInvitations: req.Body.SendInvitations, SkipRows: req.Body.SkipRows}
	for _, c := range req.Body.Columns {
		mapping.Columns = append(mapping.Columns, peopleimport.Column{Column: c.Column, Target: strings.TrimSpace(c.Target)})
	}
	run, err := s.opts.Imports.Preview(ctx, actor, req.RunId, mapping)
	if err != nil {
		return nil, importError(err)
	}
	out, err := importResponse(run)
	if err != nil {
		return nil, err
	}
	return apigen.PreviewPeopleImport200JSONResponse(out), nil
}
func (s *server) ApplyPeopleImport(ctx context.Context, req apigen.ApplyPeopleImportRequestObject) (apigen.ApplyPeopleImportResponseObject, error) {
	if s.opts.Imports == nil {
		return nil, errors.New("api: imports are not configured")
	}
	actor, err := s.importActor(ctx)
	if err != nil {
		return nil, err
	}
	run, err := s.opts.Imports.Apply(ctx, actor, req.RunId)
	if err != nil {
		return nil, importError(err)
	}
	out, err := importResponse(run)
	if err != nil {
		return nil, err
	}
	return apigen.ApplyPeopleImport200JSONResponse(out), nil
}
func (s *server) ExportPeopleImportErrors(ctx context.Context, req apigen.ExportPeopleImportErrorsRequestObject) (apigen.ExportPeopleImportErrorsResponseObject, error) {
	if s.opts.Imports == nil {
		return nil, errors.New("api: imports are not configured")
	}
	actor, err := s.importActor(ctx)
	if err != nil {
		return nil, err
	}
	run, err := s.opts.Imports.Get(ctx, actor, req.RunId)
	if err != nil {
		return nil, importError(err)
	}
	return apigen.ExportPeopleImportErrors200TextcsvResponse{Body: strings.NewReader(peopleimport.ErrorsCSV(run))}, nil
}
