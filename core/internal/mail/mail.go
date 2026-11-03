// Package mail sends system email: invitations, notifications and test
// messages (ADR 0014), separate from employee mailboxes.
//
// Messages go through an outbox (table email_outbox) worked by the job
// queue: EnqueueTx stores a message and its delivery job in the caller's
// transaction, so a message is queued exactly when the change that caused it
// commits. The worker delivers it over SMTP, retrying with a doubling delay
// (email.retry_delay, email.max_attempts) and recording every failure on the
// outbox row, where the UI reads it. Queued content is encrypted with a key
// derived from the master secret (it carries invitation links) and cleared
// once the message leaves the queue.
//
// The SMTP server comes from the email settings an Owner or Admin saves in
// the web UI, with the password encrypted at rest, or from the configuration
// (email.smtp.*), which then takes precedence. Without either, nothing can
// be sent and invitations produce links to share instead.
package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/config"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/store"
)

// Errors returned by Service.
var (
	// ErrNotConfigured: no SMTP server is set, neither in the settings nor
	// in the configuration.
	ErrNotConfigured = errors.New("mail: system email is not configured")
	// ErrManagedByConfig: the SMTP server is set in the configuration, so
	// the settings cannot be changed in the web UI.
	ErrManagedByConfig = errors.New("mail: system email is managed by the server configuration")
	// ErrInvalid wraps invalid settings with the offending field.
	ErrInvalid = errors.New("mail: invalid settings")
	// ErrNoQueue: the job queue is not attached (see SetQueue).
	ErrNoQueue = errors.New("mail: the job queue is not running")
)

// Source says where the SMTP settings come from.
type Source string

// Settings sources.
const (
	SourceNone     Source = "none"
	SourceSettings Source = "settings"
	SourceConfig   Source = "config"
)

// Settings are the effective SMTP settings.
type Settings struct {
	Source Source
	SMTP   SMTP
}

// Queue inserts jobs in a transaction; *river.Client is one.
type Queue interface {
	InsertTx(ctx context.Context, tx pgx.Tx, args river.JobArgs, opts *river.InsertOpts) (*rivertype.JobInsertResult, error)
}

// Service sends system email.
type Service struct {
	pool     *pgxpool.Pool
	cfg      config.Email
	helo     string
	settings *crypto.Sealer
	outbox   *crypto.Sealer
	logger   *slog.Logger

	mu    sync.RWMutex
	queue Queue
}

// NewService returns the mail service. helo is the name the core greets
// SMTP servers with, the workspace host name.
func NewService(pool *pgxpool.Pool, cfg config.Email, helo string, keyring *crypto.Keyring, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		pool:     pool,
		cfg:      cfg,
		helo:     helo,
		settings: keyring.Sealer(crypto.PurposeEmailSettings),
		outbox:   keyring.Sealer(crypto.PurposeEmailOutbox),
		logger:   logger,
	}
}

// SetQueue attaches the job queue that delivers queued email. The job
// queue's worker needs the service, so it is attached after both exist.
func (s *Service) SetQueue(q Queue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queue = q
}

// settingsKey is the instance_settings key of the email settings.
const settingsKey = "email"

// passwordAD binds the sealed SMTP password to its place.
var passwordAD = []byte("email-settings:smtp-password")

type storedSettings struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	TLS      string `json:"tls"`
	Username string `json:"username,omitempty"`
	// Password is sealed with PurposeEmailSettings.
	Password []byte `json:"password,omitempty"`
	From     string `json:"from"`
}

// Settings returns the effective SMTP settings: from the configuration if it
// sets a host, otherwise the saved settings, otherwise SourceNone.
func (s *Service) Settings(ctx context.Context) (Settings, error) {
	if s.cfg.Configured() {
		return Settings{Source: SourceConfig, SMTP: SMTP{
			Host: s.cfg.SMTP.Host, Port: s.cfg.SMTP.Port, TLS: s.cfg.SMTP.TLS,
			Username: s.cfg.SMTP.Username, Password: s.cfg.SMTP.Password, From: s.cfg.From,
		}}, nil
	}
	stored, ok, err := s.loadStored(ctx, store.New(s.pool))
	if err != nil || !ok {
		return Settings{Source: SourceNone}, err
	}
	smtp := SMTP{Host: stored.Host, Port: stored.Port, TLS: stored.TLS, Username: stored.Username, From: stored.From}
	if len(stored.Password) > 0 {
		pw, err := s.settings.Open(stored.Password, passwordAD)
		if err != nil {
			return Settings{}, fmt.Errorf("mail: decrypt the SMTP password (was identity.master_secret changed?): %w", err)
		}
		smtp.Password = string(pw)
	}
	return Settings{Source: SourceSettings, SMTP: smtp}, nil
}

// Configured reports whether system email can be sent.
func (s *Service) Configured(ctx context.Context) (bool, error) {
	if s.cfg.Configured() {
		return true, nil
	}
	_, ok, err := s.loadStored(ctx, store.New(s.pool))
	return ok, err
}

func (s *Service) loadStored(ctx context.Context, q *store.Queries) (storedSettings, bool, error) {
	raw, err := q.GetInstanceSetting(ctx, settingsKey)
	if errors.Is(err, pgx.ErrNoRows) {
		return storedSettings{}, false, nil
	} else if err != nil {
		return storedSettings{}, false, fmt.Errorf("mail: load settings: %w", err)
	}
	var st storedSettings
	if err := json.Unmarshal(raw, &st); err != nil {
		return storedSettings{}, false, fmt.Errorf("mail: decode settings: %w", err)
	}
	return st, true, nil
}

// SettingsInput are SMTP settings to save.
type SettingsInput struct {
	Host string
	// Port 0 picks the usual port of the TLS mode.
	Port     int
	TLS      string
	Username string
	// Password nil keeps the saved password (if the username is
	// unchanged); an empty string clears it.
	Password *string
	From     string
}

const (
	maxUsernameLength = 254
	maxPasswordLength = 1024
)

func invalid(field, format string, args ...any) error {
	return fmt.Errorf("%w: %s: %s", ErrInvalid, field, fmt.Sprintf(format, args...))
}

// PutSettings validates and saves the SMTP settings, with an audit entry
// listing what changed (never the password).
func (s *Service) PutSettings(ctx context.Context, actor audit.Actor, in SettingsInput) error {
	if s.cfg.Configured() {
		return ErrManagedByConfig
	}
	next := storedSettings{
		Host:     strings.TrimSuffix(strings.ToLower(strings.TrimSpace(in.Host)), "."),
		Port:     in.Port,
		TLS:      strings.ToLower(strings.TrimSpace(in.TLS)),
		Username: strings.TrimSpace(in.Username),
		From:     strings.TrimSpace(in.From),
	}
	if next.TLS == "" {
		next.TLS = config.SMTPStartTLS
	}
	if !slices.Contains([]string{config.SMTPStartTLS, config.SMTPImplicitTLS, config.SMTPNoTLS}, next.TLS) {
		return invalid("tls", "must be %s, %s or %s", config.SMTPStartTLS, config.SMTPImplicitTLS, config.SMTPNoTLS)
	}
	if next.Port == 0 {
		next.Port = config.DefaultSMTPPort(next.TLS)
	}
	if next.Port < 1 || next.Port > 65535 {
		return invalid("port", "must be between 1 and 65535")
	}
	if !validHost(next.Host) {
		return invalid("host", "must be a host name or IP address, e.g. smtp.example.com")
	}
	if len(next.Username) > maxUsernameLength || strings.ContainsAny(next.Username, "\r\n") {
		return invalid("username", "must be at most %d characters on one line", maxUsernameLength)
	}
	if next.From == "" {
		return invalid("from", "required, e.g. Acme <no-reply@acme.com>")
	}
	from, err := ParseAddress(next.From)
	if err != nil || utf8.RuneCountInString(next.From) > 320 {
		return invalid("from", "must be an address such as no-reply@acme.com or Acme <no-reply@acme.com>")
	}
	next.From = from.String()
	if in.Password != nil && (len(*in.Password) > maxPasswordLength || strings.ContainsAny(*in.Password, "\r\n")) {
		return invalid("password", "must be at most %d bytes on one line", maxPasswordLength)
	}

	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := q.AcquireXactLock(ctx, settingsLockID); err != nil {
			return fmt.Errorf("mail: settings lock: %w", err)
		}
		current, existed, err := s.loadStored(ctx, q)
		if err != nil {
			return err
		}
		switch {
		case in.Password != nil && *in.Password != "":
			next.Password = s.settings.Seal([]byte(*in.Password), passwordAD)
		case in.Password == nil && existed && current.Username == next.Username:
			next.Password = current.Password
		}
		if next.Username != "" && len(next.Password) == 0 {
			return invalid("password", "required with a username")
		}
		if next.Username == "" {
			next.Password = nil
		}
		var changed []string
		for _, f := range []struct {
			name string
			diff bool
		}{
			{"host", current.Host != next.Host},
			{"port", current.Port != next.Port},
			{"tls", current.TLS != next.TLS},
			{"username", current.Username != next.Username},
			{"password", in.Password != nil || (len(current.Password) > 0) != (len(next.Password) > 0)},
			{"from", current.From != next.From},
		} {
			if f.diff || !existed {
				changed = append(changed, f.name)
			}
		}
		raw, err := json.Marshal(next) //nolint:gosec // Password contains only AEAD ciphertext.
		if err != nil {
			return err
		}
		if err := q.PutInstanceSetting(ctx, store.PutInstanceSettingParams{Key: settingsKey, Value: raw}); err != nil {
			return fmt.Errorf("mail: save settings: %w", err)
		}
		_, err = audit.Record(ctx, q, audit.Entry{
			Actor:  actor,
			Action: audit.EmailSettingsUpdated,
			Metadata: map[string]any{
				"fields": changed, "host": next.Host, "port": next.Port, "tls": next.TLS, "from": next.From,
				"authentication": next.Username != "",
			},
		})
		return err
	})
}

// settingsLockID serializes changes to the email settings.
const settingsLockID int64 = 0x6f72676c6d61696c // "orglmail"

// DeleteSettings removes the saved SMTP settings. Invitations produce links
// to share afterwards. Removing settings that do not exist is a no-op.
func (s *Service) DeleteSettings(ctx context.Context, actor audit.Actor) error {
	if s.cfg.Configured() {
		return ErrManagedByConfig
	}
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		n, err := q.DeleteInstanceSetting(ctx, settingsKey)
		if err != nil || n == 0 {
			return err
		}
		_, err = audit.Record(ctx, q, audit.Entry{Actor: actor, Action: audit.EmailSettingsRemoved})
		return err
	})
}

// SendTest queues and audits a test message, then waits for its first
// delivery result so the settings screen can show an SMTP error. The worker
// retains responsibility for retries, just as for other system email.
func (s *Service) SendTest(ctx context.Context, actor audit.Actor, to, locale string, data Test) error {
	rcpt, err := ParseRecipient(to)
	if err != nil {
		return invalid("to", "must be an address such as name@example.com")
	}
	settings, err := s.Settings(ctx)
	if err != nil {
		return err
	}
	if settings.Source == SourceNone {
		return ErrNotConfigured
	}
	msg, err := RenderTest(locale, data)
	if err != nil {
		return err
	}
	var id uuid.UUID
	if err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var err error
		id, err = s.EnqueueTx(ctx, tx, KindTest, rcpt, msg)
		if err != nil {
			return err
		}
		_, err = audit.Record(ctx, store.New(tx), audit.Entry{Actor: actor, Action: audit.EmailSettingsTested, Metadata: map[string]any{"source": settings.Source, "email_id": id}})
		return err
	}); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, sendTimeout+10*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		row, err := store.New(s.pool).GetOutboxEmail(ctx, id)
		if err != nil {
			return err
		}
		if Status(row.Status) == StatusSent {
			return nil
		}
		if row.LastError != nil {
			return &SendError{Err: errors.New(*row.LastError)}
		}
		select {
		case <-ctx.Done():
			return &SendError{Err: errors.New("the test email is still queued; check delivery later")}
		case <-ticker.C:
		}
	}
}

// SendError is a failed delivery: the SMTP server could not be reached or
// rejected the message. Its text is meant for the person configuring email.
type SendError struct {
	Err error
}

func (e *SendError) Error() string { return truncate(e.Err.Error()) }
func (e *SendError) Unwrap() error { return e.Err }

// Status is the delivery state of a queued email.
type Status string

// Outbox statuses.
const (
	StatusQueued   Status = "queued"
	StatusSent     Status = "sent"
	StatusFailed   Status = "failed"
	StatusCanceled Status = "canceled"
)

// SendArgs are the arguments of the delivery job of one outbox email.
type SendArgs struct {
	EmailID uuid.UUID `json:"email_id"`
}

// Kind implements river.JobArgs.
func (SendArgs) Kind() string { return "orglane.send_email" }

// EnqueueTx stores a message for to in the outbox and inserts its delivery
// job, both in tx: the message is sent only if tx commits. kind names the
// message, e.g. KindInvitation.
func (s *Service) EnqueueTx(ctx context.Context, tx pgx.Tx, kind, to string, m Message) (uuid.UUID, error) {
	s.mu.RLock()
	queue := s.queue
	s.mu.RUnlock()
	if queue == nil {
		return uuid.Nil, ErrNoQueue
	}
	rcpt, err := ParseRecipient(to)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.Nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return uuid.Nil, err
	}
	if _, err := store.New(tx).CreateOutboxEmail(ctx, store.CreateOutboxEmailParams{
		ID: id, Kind: kind, Recipient: rcpt, SealedMessage: s.outbox.Seal(raw, id[:]), MaxAttempts: int32(s.cfg.MaxAttempts), //nolint:gosec // validated to 1-25
	}); err != nil {
		return uuid.Nil, fmt.Errorf("mail: queue message: %w", err)
	}
	if _, err := queue.InsertTx(ctx, tx, SendArgs{EmailID: id}, &river.InsertOpts{MaxAttempts: s.cfg.MaxAttempts}); err != nil {
		return uuid.Nil, fmt.Errorf("mail: queue delivery job: %w", err)
	}
	return id, nil
}

// CancelTx cancels a queued email that is no longer wanted (an invitation
// was resent or revoked) and clears its content. Sent or failed emails are
// left as they are.
func CancelTx(ctx context.Context, q *store.Queries, id uuid.UUID) error {
	if _, err := q.CancelOutboxEmail(ctx, id); err != nil {
		return fmt.Errorf("mail: cancel message: %w", err)
	}
	return nil
}

// Deliver makes delivery attempt number attempt of an outbox email. A
// failure is recorded on the row and returned, so the job queue retries;
// the last attempt marks the email failed and clears its content. Emails no
// longer queued (sent, failed, canceled) are skipped.
func (s *Service) Deliver(ctx context.Context, id uuid.UUID, attempt int) error {
	q := store.New(s.pool)
	row, err := q.GetOutboxEmail(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	} else if err != nil {
		return fmt.Errorf("mail: load message: %w", err)
	}
	if Status(row.Status) != StatusQueued {
		return nil
	}
	sendErr := func() error {
		raw, err := s.outbox.Open(row.SealedMessage, id[:])
		if err != nil {
			return fmt.Errorf("decrypt the queued message (was identity.master_secret changed?): %w", err)
		}
		var m Message
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		settings, err := s.Settings(ctx)
		if err != nil {
			return err
		}
		if settings.Source == SourceNone {
			return errors.New("system email is not configured")
		}
		return send(ctx, settings.SMTP, s.helo, row.Recipient, m)
	}()
	attempts := int32(min(attempt, 1<<20)) //nolint:gosec // bounded
	if sendErr == nil {
		if err := q.MarkOutboxEmailSent(ctx, store.MarkOutboxEmailSentParams{ID: id, Attempts: attempts}); err != nil {
			return fmt.Errorf("mail: record delivery: %w", err)
		}
		s.logger.InfoContext(ctx, "email sent", "email_id", id, "kind", row.Kind, "attempt", attempt)
		return nil
	}
	final := attempt >= int(row.MaxAttempts)
	msg := truncate(sendErr.Error())
	if err := q.RecordOutboxEmailFailure(ctx, store.RecordOutboxEmailFailureParams{ID: id, Attempts: attempts, LastError: &msg, Final: final}); err != nil {
		return fmt.Errorf("mail: record failure: %w", err)
	}
	s.logger.WarnContext(ctx, "email delivery failed", "email_id", id, "kind", row.Kind, "attempt", attempt,
		"max_attempts", row.MaxAttempts, "final", final, "error", msg)
	if final {
		return river.JobCancel(sendErr)
	}
	return sendErr
}

// RetryDelay is the wait after failed attempt number attempt: the base
// delay, doubling with every attempt, at most an hour.
func RetryDelay(base time.Duration, attempt int) time.Duration {
	d := base
	for i := 1; i < attempt && d < time.Hour; i++ {
		d *= 2
	}
	return min(d, time.Hour)
}

// Worker delivers outbox emails (SendArgs).
type Worker struct {
	river.WorkerDefaults[SendArgs]
	Service *Service
}

// Work implements river.Worker.
func (w *Worker) Work(ctx context.Context, job *river.Job[SendArgs]) error {
	return w.Service.Deliver(ctx, job.Args.EmailID, job.Attempt)
}

// NextRetry implements river.Worker.
func (w *Worker) NextRetry(job *river.Job[SendArgs]) time.Time {
	return time.Now().Add(RetryDelay(w.Service.cfg.RetryDelay, job.Attempt))
}

// Timeout implements river.Worker.
func (w *Worker) Timeout(*river.Job[SendArgs]) time.Duration { return sendTimeout + 10*time.Second }

// maxErrorLength matches the email_outbox.last_error constraint.
const maxErrorLength = 1000

func truncate(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	if len(s) <= maxErrorLength {
		return s
	}
	cut := maxErrorLength
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func validHost(h string) bool {
	if _, err := netip.ParseAddr(h); err == nil {
		return true
	}
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return false
			}
		}
	}
	return true
}
