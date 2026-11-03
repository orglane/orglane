package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/orglane/orglane/core/internal/api/apigen"
	"github.com/orglane/orglane/core/internal/mail"
)

func (s *server) mail() (*mail.Service, error) {
	if s.opts.Mail == nil {
		return nil, errors.New("api: system email is not configured")
	}
	return s.opts.Mail, nil
}

func (s *server) GetEmailSettings(ctx context.Context, _ apigen.GetEmailSettingsRequestObject) (apigen.GetEmailSettingsResponseObject, error) {
	m, err := s.mail()
	if err != nil {
		return nil, err
	}
	out, err := emailSettings(ctx, m)
	if err != nil {
		return nil, err
	}
	return apigen.GetEmailSettings200JSONResponse(out), nil
}

func (s *server) PutEmailSettings(ctx context.Context, req apigen.PutEmailSettingsRequestObject) (apigen.PutEmailSettingsResponseObject, error) {
	m, err := s.mail()
	if err != nil {
		return nil, err
	}
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	b := req.Body
	in := mail.SettingsInput{Host: b.Host, TLS: string(b.Tls), From: b.From, Password: b.Password}
	if b.Port != nil {
		in.Port = *b.Port
	}
	if b.Username != nil {
		in.Username = *b.Username
	}
	_, r := httpFrom(ctx)
	if err := m.PutSettings(ctx, a.Actor(r, personID), in); err != nil {
		return nil, mailError(err)
	}
	out, err := emailSettings(ctx, m)
	if err != nil {
		return nil, err
	}
	return apigen.PutEmailSettings200JSONResponse(out), nil
}

func (s *server) DeleteEmailSettings(ctx context.Context, _ apigen.DeleteEmailSettingsRequestObject) (apigen.DeleteEmailSettingsResponseObject, error) {
	m, err := s.mail()
	if err != nil {
		return nil, err
	}
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	if err := m.DeleteSettings(ctx, a.Actor(r, personID)); err != nil {
		return nil, mailError(err)
	}
	return apigen.DeleteEmailSettings204Response{}, nil
}

func (s *server) SendTestEmail(ctx context.Context, req apigen.SendTestEmailRequestObject) (apigen.SendTestEmailResponseObject, error) {
	m, err := s.mail()
	if err != nil {
		return nil, err
	}
	a, err := s.auth()
	if err != nil {
		return nil, err
	}
	personID, err := callerID(ctx)
	if err != nil {
		return nil, err
	}
	sender, err := a.Identity().GetPerson(ctx, personID)
	if err != nil {
		return nil, err
	}
	_, r := httpFrom(ctx)
	err = m.SendTest(ctx, a.Actor(r, personID), strings.TrimSpace(req.Body.To), r.Header.Get("Accept-Language"),
		mail.Test{Workspace: s.opts.WorkspaceHost, Sender: sender.DisplayName})
	if err != nil {
		return nil, mailError(err)
	}
	return apigen.SendTestEmail204Response{}, nil
}

func emailSettings(ctx context.Context, m *mail.Service) (apigen.EmailSettings, error) {
	st, err := m.Settings(ctx)
	if err != nil {
		return apigen.EmailSettings{}, err
	}
	out := apigen.EmailSettings{Source: apigen.EmailSettingsSource(st.Source)}
	if st.Source == mail.SourceNone {
		return out, nil
	}
	smtp := st.SMTP
	tls := apigen.EmailTLSMode(smtp.TLS)
	passwordSet := smtp.Password != ""
	out.Host, out.Port, out.Tls, out.Username, out.PasswordSet, out.From = &smtp.Host, &smtp.Port, &tls, &smtp.Username, &passwordSet, &smtp.From
	return out, nil
}

// mailError maps mail failures to responses.
func mailError(err error) error {
	var sendErr *mail.SendError
	switch {
	case errors.Is(err, mail.ErrInvalid):
		return problemErr(http.StatusBadRequest, strings.TrimPrefix(err.Error(), mail.ErrInvalid.Error()+": "))
	case errors.Is(err, mail.ErrManagedByConfig):
		return problemErr(http.StatusConflict, "System email is set in the server configuration (email.smtp.*) and cannot be changed here.")
	case errors.Is(err, mail.ErrNotConfigured):
		return problemErr(http.StatusConflict, "System email is not configured yet. Save the SMTP settings first.")
	case errors.As(err, &sendErr):
		return problemErr(http.StatusBadGateway, sendErr.Error())
	default:
		return err
	}
}
