package peopleimport

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/store"
)

// Retention is fixed by the maintainer decision on issue #42; these are
// constants, not settings (docs/specs/people-import.md "Lifecycle").
const (
	// StagingRetention bounds how long an unapplied (staged or previewed) run
	// and its encrypted source material exist after upload.
	StagingRetention = 24 * time.Hour
	// AppliedRetention bounds how long the value-free result of an applied
	// run exists after apply.
	AppliedRetention = 90 * 24 * time.Hour

	cleanupBatch = 100
)

// Audit actions of the import lifecycle. Expiry and erasure of legacy source
// material are recorded with the system actor, like other maintenance jobs.
const (
	actionDeleted     audit.Action = "import.deleted"
	actionExpired     audit.Action = "import.expired"
	actionSourceErase audit.Action = "import.source_erased"
)

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// captions returns the value-free column captions for a run: the stored ones
// once the source material was erased, otherwise derived from the sealed
// headers. Unknown headings may themselves contain sensitive data; only known
// canonical field names are returned and clients number everything else.
func (s *Service) captions(r store.PeopleImportRun) ([]string, error) {
	if r.HeadersSealed == nil {
		if r.Captions == nil {
			return nil, errors.New("import: run has neither headers nor captions")
		}
		return r.Captions, nil
	}
	var headers []string
	if err := s.open(r.ID, 0, "headers", r.HeadersSealed, &headers); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(headers))
	for _, h := range headers {
		out = append(out, headingTarget(h))
	}
	return out, nil
}

// eraseSourceTx removes every sealed value of an applied run (headers, row
// payloads and previews, including unmapped, skipped and conflicted cells),
// keeping captions, mapping, decisions, error codes and results.
func (s *Service) eraseSourceTx(ctx context.Context, q *store.Queries, r store.PeopleImportRun) (store.PeopleImportRun, error) {
	captions, err := s.captions(r)
	if err != nil {
		return store.PeopleImportRun{}, err
	}
	if err = q.ErasePeopleImportRows(ctx, r.ID); err != nil {
		return store.PeopleImportRun{}, err
	}
	return q.ErasePeopleImportRun(ctx, store.ErasePeopleImportRunParams{ID: r.ID, Captions: captions})
}

// Delete removes a run and everything stored for it. Any current holder of
// people.manage may delete any run, the uploader included. Deleting a run that
// does not exist succeeds without an audit entry. People and changes made by
// an applied run stay; so do the source-scoped identity matches.
func (s *Service) Delete(ctx context.Context, actor audit.Actor, id uuid.UUID) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		// The grant lock serializes with upload, read, preview and apply, so
		// a run is never deleted halfway through being applied.
		if err := identity.LockGrantsTx(ctx, q); err != nil {
			return err
		}
		if _, err := authorized(ctx, q, actor); err != nil {
			return err
		}
		deleted, err := q.DeletePeopleImportRun(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return record(ctx, q, actor, deleted.ID, actionDeleted, map[string]any{"state": deleted.State})
	})
}

// CleanupResult counts what one Cleanup call did.
type CleanupResult struct {
	// Expired is the number of runs deleted because they expired.
	Expired int
	// Erased is the number of runs applied by an earlier version whose
	// source material was erased.
	Erased int
	// Unreadable is the number of erased runs whose sealed headers could not
	// be opened; they got numbered captions instead.
	Unreadable int
}

// Cleanup deletes expired runs and erases source material left by runs
// applied before apply erased it. The import cleanup job runs it hourly. It is
// idempotent. Every transaction takes the grant lock that upload, read,
// preview, apply and delete take as well. Expiry runs in batches of its own,
// and each earlier run is erased in its own transaction, so a run that cannot
// be erased never holds back the deletion of expired runs.
func (s *Service) Cleanup(ctx context.Context) (CleanupResult, error) {
	var res CleanupResult
	for {
		n := 0
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := store.New(tx)
			if err := identity.LockGrantsTx(ctx, q); err != nil {
				return err
			}
			expired, err := q.DeleteExpiredPeopleImportRuns(ctx, cleanupBatch)
			if err != nil {
				return err
			}
			for _, r := range expired {
				if err = record(ctx, q, audit.System, r.ID, actionExpired, map[string]any{"state": r.State}); err != nil {
					return err
				}
			}
			n = len(expired)
			return nil
		})
		if err != nil {
			return res, err
		}
		res.Expired += n
		if n == 0 {
			break
		}
	}
	for {
		erased, unreadable := false, false
		err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			q := store.New(tx)
			if err := identity.LockGrantsTx(ctx, q); err != nil {
				return err
			}
			legacy, err := q.ListUnerasedAppliedPeopleImportRuns(ctx, 1)
			if err != nil || len(legacy) == 0 {
				return err
			}
			r := legacy[0]
			captions, err := s.captions(r)
			if err != nil {
				// Unreadable ciphertext is erased all the same; only the error
				// class is logged, never contents.
				s.logger.WarnContext(ctx, "import cleanup: sealed headers of an applied run cannot be opened; erasing with numbered captions",
					"run_id", r.ID, "error_class", errorClass(err))
				captions, unreadable = numberedCaptions(r), true
			}
			if err = q.ErasePeopleImportRows(ctx, r.ID); err != nil {
				return err
			}
			if _, err = q.ErasePeopleImportRun(ctx, store.ErasePeopleImportRunParams{ID: r.ID, Captions: captions}); err != nil {
				return err
			}
			metadata := map[string]any{}
			if unreadable {
				metadata["headers_unreadable"] = true
			}
			erased = true
			return record(ctx, q, audit.System, r.ID, actionSourceErase, metadata)
		})
		if err != nil {
			return res, err
		}
		if !erased {
			return res, nil
		}
		res.Erased++
		if unreadable {
			res.Unreadable++
		}
	}
}

// numberedCaptions covers every mapped column with an unknown (numbered)
// caption when the original headers cannot be read.
func numberedCaptions(r store.PeopleImportRun) []string {
	var m Mapping
	n := 0
	if json.Unmarshal(r.Mapping, &m) == nil {
		for _, c := range m.Columns {
			if c.Column >= n && c.Column < MaxColumns {
				n = c.Column + 1
			}
		}
	}
	return make([]string, n)
}

func errorClass(err error) string {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntax), errors.As(err, &typeErr):
		return "invalid_json"
	default:
		return "decrypt_failed"
	}
}
