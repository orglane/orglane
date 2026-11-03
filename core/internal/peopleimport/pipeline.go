package peopleimport

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/crypto"
	"github.com/orglane/orglane/core/internal/identity"
	"github.com/orglane/orglane/core/internal/profile"
	"github.com/orglane/orglane/core/internal/store"
)

// Stable errors never contain file contents or database constraint details.
var (
	ErrInvalid   = errors.New("import: invalid mapping or file")
	ErrNotFound  = errors.New("import: run not found")
	ErrState     = errors.New("import: preview required or run already applied")
	ErrForbidden = errors.New("import: permission denied")
)

// Column maps a zero-based source column to a domain field.
type Column struct {
	Column int    `json:"column"`
	Target string `json:"target"`
}

// Mapping is persisted independently from encrypted source records.
type Mapping struct {
	Columns         []Column `json:"columns"`
	CreateTeams     bool     `json:"createTeams"`
	SendInvitations bool     `json:"sendInvitations"`
	SkipRows        []int    `json:"skipRows"`
}

// Row is the value-free public decision and result for one source row.
type Row struct {
	Row      int    `json:"row"`
	Decision string `json:"decision"`
	Error    string `json:"error"`
	Result   string `json:"result"`
}

// Run is safe to disclose to its current authorized uploader after commit.
type Run struct {
	ID        uuid.UUID `json:"id"`
	State     string    `json:"state"`
	Source    string    `json:"source"`
	Encoding  string    `json:"encoding"`
	Delimiter string    `json:"delimiter"`
	Headers   []string  `json:"headers"`
	Mapping   Mapping   `json:"mapping"`
	Rows      []Row     `json:"rows"`
	CreatedAt time.Time `json:"createdAt"`
}

// Service persists runs and delegates mutations to identity/profile domains.
type Service struct {
	pool        *pgxpool.Pool
	identity    *identity.Service
	profile     *profile.Service
	sealer      *crypto.Sealer
	identityKey [32]byte
}

// NewService uses purpose-separated authenticated encryption for staging.
func NewService(pool *pgxpool.Pool, ids *identity.Service, profiles *profile.Service, keys *crypto.Keyring) *Service {
	return &Service{pool: pool, identity: ids, profile: profiles, sealer: keys.Sealer(crypto.PurposeImportStaging), identityKey: keys.Key(crypto.PurposeImportIdentifiers)}
}

func emptyMapping() Mapping { return Mapping{Columns: []Column{}, SkipRows: []int{}} }
func ad(id uuid.UUID, n int, kind string) []byte {
	return []byte("people_import:" + id.String() + ":" + strconv.Itoa(n) + ":" + kind)
}
func (s *Service) seal(id uuid.UUID, n int, kind string, v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return s.sealer.Seal(b, ad(id, n, kind)), nil
}
func (s *Service) open(id uuid.UUID, n int, kind string, b []byte, v any) error {
	plain, err := s.sealer.Open(b, ad(id, n, kind))
	if err != nil {
		return err
	}
	return json.Unmarshal(plain, v)
}
func (s *Service) externalHash(v string) []byte {
	m := hmac.New(sha256.New, s.identityKey[:])
	_, _ = m.Write([]byte(strings.TrimSpace(v)))
	return m.Sum(nil)
}

func authorized(ctx context.Context, q *store.Queries, actor audit.Actor) (uuid.UUID, error) {
	p := authz.PrincipalFrom(ctx)
	if p == nil || !p.Has(authz.PeopleManage) {
		return uuid.Nil, ErrForbidden
	}
	id, err := uuid.Parse(p.Subject)
	if err != nil {
		return uuid.Nil, ErrForbidden
	}
	if err = identity.AuthorizeGrantTx(ctx, q, actor, uuid.Nil, nil); err != nil {
		return uuid.Nil, ErrForbidden
	}
	return id, nil
}

// Upload parses before starting a transaction, then encrypts every staged cell.
func (s *Service) Upload(ctx context.Context, actor audit.Actor, source Source, input io.Reader) (Run, error) {
	// Check before spending parser CPU as well as inside the grant lock.
	if !authz.PrincipalFrom(ctx).Has(authz.PeopleManage) {
		return Run{}, ErrForbidden
	}
	data, err := source.Read(input)
	if err != nil {
		return Run{}, ErrFile
	}
	if source.Key() == "" || len(source.Key()) > 50 || len(data.Rows) == 0 || len(data.Rows) > MaxRows || len(data.Headers) == 0 || !validCells(data.Headers) {
		return Run{}, ErrFile
	}
	id := uuid.Must(uuid.NewV7())
	var out Run
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := identity.LockGrantsTx(ctx, q); err != nil {
			return err
		}
		caller, err := authorized(ctx, q, actor)
		if err != nil {
			return err
		}
		sealed, err := s.seal(id, 0, "headers", data.Headers)
		if err != nil {
			return err
		}
		run, err := q.CreatePeopleImportRun(ctx, store.CreatePeopleImportRunParams{ID: id, CreatedBy: caller, Source: source.Key(), Encoding: data.Encoding, Delimiter: data.Delimiter, HeadersSealed: sealed})
		if err != nil {
			return err
		}
		last := 1
		for _, row := range data.Rows {
			if row.Number < 2 || row.Number <= last || row.Number > 1048576 || !validCells(row.Cells) {
				return ErrFile
			}
			last = row.Number
			// Source adapters may only return known non-value-bearing error codes.
			if row.Error != "" && !slices.Contains([]string{"column_count", "cell_limit", "formula", "cell_error"}, row.Error) {
				return ErrFile
			}
			sealed, err := s.seal(id, row.Number, "row", row)
			if err != nil {
				return err
			}
			if err = q.InsertPeopleImportRow(ctx, store.InsertPeopleImportRowParams{RunID: id, RowNumber: int32(row.Number), PayloadSealed: sealed, ErrorCode: row.Error}); err != nil {
				return err
			}
		}
		if err = record(ctx, q, actor, id, "import.created", map[string]any{"rows": len(data.Rows), "source": source.Key()}); err != nil {
			return err
		}
		out, err = s.view(ctx, q, run)
		return err
	})
	return out, err
}

func record(ctx context.Context, q *store.Queries, actor audit.Actor, id uuid.UUID, action audit.Action, metadata map[string]any) error {
	_, err := audit.Record(ctx, q, audit.Entry{Actor: actor, Action: action, Target: audit.Target{Kind: "people_import", ID: id.String()}, Metadata: metadata})
	return err
}

// Get returns persisted value-free decisions, restricted to the uploader.
func (s *Service) Get(ctx context.Context, actor audit.Actor, id uuid.UUID) (Run, error) {
	var out Run
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := identity.LockGrantsTx(ctx, q); err != nil {
			return err
		}
		caller, err := authorized(ctx, q, actor)
		if err != nil {
			return err
		}
		run, err := q.GetPeopleImportRun(ctx, store.GetPeopleImportRunParams{ID: id, CreatedBy: caller})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out, err = s.view(ctx, q, run)
		return err
	})
	return out, err
}

var baseTargets = []string{"username", "work_email", "contact_email", "display_name", "title", "teams", "role", "manager_email", "external_id"}

func headingTarget(h string) string {
	h = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(h), " ", "_"))
	switch h {
	case "name", "full_name":
		return "display_name"
	case "email":
		return "work_email"
	case "manager":
		return "manager_email"
	case "team":
		return "teams"
	}
	if slices.Contains(baseTargets, h) {
		return h
	}
	return ""
}
func (s *Service) view(ctx context.Context, q *store.Queries, r store.PeopleImportRun) (Run, error) {
	out := Run{ID: r.ID, State: r.State, Source: r.Source, Encoding: r.Encoding, Delimiter: r.Delimiter, Mapping: emptyMapping(), Headers: []string{}, Rows: []Row{}, CreatedAt: r.CreatedAt}
	if err := json.Unmarshal(r.Mapping, &out.Mapping); err != nil {
		return Run{}, err
	}
	var headers []string
	if err := s.open(r.ID, 0, "headers", r.HeadersSealed, &headers); err != nil {
		return Run{}, err
	}
	// Unknown headings may themselves contain sensitive data. Return only known
	// canonical field captions; clients label everything else by column number.
	for _, h := range headers {
		out.Headers = append(out.Headers, headingTarget(h))
	}
	rows, err := q.ListPeopleImportRows(ctx, r.ID)
	if err != nil {
		return Run{}, err
	}
	for _, row := range rows {
		out.Rows = append(out.Rows, Row{Row: int(row.RowNumber), Decision: row.Decision, Error: row.ErrorCode, Result: row.Result})
	}
	return out, nil
}

func (s *Service) validateMapping(ctx context.Context, q *store.Queries, run store.PeopleImportRun, m Mapping) error {
	var headers []string
	if err := s.open(run.ID, 0, "headers", run.HeadersSealed, &headers); err != nil {
		return err
	}
	if len(m.Columns) == 0 || len(m.Columns) > MaxColumns || len(m.SkipRows) > MaxRows {
		return ErrInvalid
	}
	seenC := map[int]bool{}
	seenT := map[string]bool{}
	fields, err := q.ListActiveProfileFieldsForShare(ctx)
	if err != nil {
		return err
	}
	allowed := map[string]bool{}
	sensitive, err := canReadSensitive(ctx, q, run.CreatedBy)
	if err != nil {
		return err
	}
	for _, f := range fields {
		if !f.Sensitive || sensitive {
			allowed["profile."+f.Key] = true
		}
	}
	for _, c := range m.Columns {
		if c.Column < 0 || c.Column >= len(headers) || seenC[c.Column] || seenT[c.Target] || (!slices.Contains(baseTargets, c.Target) && !allowed[c.Target]) {
			return ErrInvalid
		}
		seenC[c.Column] = true
		seenT[c.Target] = true
	}
	if !seenT["work_email"] && !seenT["contact_email"] && !seenT["external_id"] {
		return ErrInvalid
	}
	seenSkip := map[int]bool{}
	for _, n := range m.SkipRows {
		if n < 2 || n > 1048576 || seenSkip[n] {
			return ErrInvalid
		}
		seenSkip[n] = true
	}
	return nil
}

func values(rec Record, m Mapping) map[string]string {
	v := map[string]string{}
	for _, c := range m.Columns {
		if c.Column < len(rec.Cells) {
			v[c.Target] = strings.TrimSpace(rec.Cells[c.Column])
		}
	}
	return v
}

// match compares all supplied identities even when a higher-priority match is
// found: a contact email pointing to another person is never silently ignored.
func (s *Service) match(ctx context.Context, q *store.Queries, source string, v map[string]string) (uuid.UUID, error) {
	id := uuid.Nil
	candidates := [][]store.Person{}
	if v["work_email"] != "" {
		p, e := q.FindPeopleImportWorkEmail(ctx, v["work_email"])
		if e != nil {
			return id, e
		}
		candidates = append(candidates, p)
	}
	if v["contact_email"] != "" {
		p, e := q.FindPeopleImportContactEmail(ctx, v["contact_email"])
		if e != nil {
			return id, e
		}
		candidates = append(candidates, p)
	}
	if v["external_id"] != "" {
		p, e := q.FindPeopleImportExternalID(ctx, store.FindPeopleImportExternalIDParams{Source: source, ExternalHash: s.externalHash(v["external_id"])})
		if e != nil {
			return id, e
		}
		candidates = append(candidates, p)
	}
	for _, people := range candidates {
		if len(people) > 1 {
			return uuid.Nil, errConflict
		}
		if len(people) == 1 {
			if id != uuid.Nil && id != people[0].ID {
				return uuid.Nil, errConflict
			}
			id = people[0].ID
		}
	}
	return id, nil
}

var errConflict = errors.New("identity_conflict")
var errStale = errors.New("stale_preview")

// previewState contains internal IDs, availability and hashes; it stays encrypted.
type previewState struct {
	ID        uuid.UUID
	ManagerID uuid.UUID
	Version   []byte
	Teams     []teamResolution
}

// An empty resolution records absence. Candidate IDs and availability capture
// both unambiguous and ambiguous names without storing source values here.
type teamResolution []teamCandidate
type teamCandidate struct {
	ID       uuid.UUID
	Archived bool
}

func teamNames(raw string) []string {
	var names []string
	for _, name := range strings.Split(raw, ";") {
		if name = strings.TrimSpace(name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

func sameTeams(a, b []teamResolution) bool {
	return slices.EqualFunc(a, b, slices.Equal[teamResolution])
}

// Resolve source names under the import locks before any row writes. SQL owns
// case matching. In particular, a missing team stays absent in this snapshot
// even if an earlier row of this same apply subsequently auto-creates it.
func frozenTeamResolutions(ctx context.Context, q *store.Queries, records []Record, m Mapping, duplicate map[int]bool) (map[int][]teamResolution, error) {
	frozen := make(map[int][]teamResolution, len(records))
	cache := map[string]teamResolution{}
	for _, rec := range records {
		if rec.Error != "" || duplicate[rec.Number] || slices.Contains(m.SkipRows, rec.Number) {
			continue
		}
		names := teamNames(values(rec, m)["teams"])
		if len(names) > 100 {
			// The row mutation reports the domain limit.
			continue
		}
		resolutions := make([]teamResolution, 0, len(names))
		for _, name := range names {
			found, ok := cache[name]
			if !ok {
				teams, err := q.FindPeopleImportTeams(ctx, name)
				if err != nil {
					return nil, err
				}
				found = make(teamResolution, 0, len(teams))
				for _, team := range teams {
					found = append(found, teamCandidate{ID: team.ID, Archived: team.ArchivedAt != nil})
				}
				cache[name] = found
			}
			resolutions = append(resolutions, found)
		}
		frozen[rec.Number] = resolutions
	}
	return frozen, nil
}

func snapshot(ctx context.Context, q *store.Queries, id uuid.UUID) ([]byte, error) {
	b, err := q.PeopleImportSnapshot(ctx, id)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}
func (s *Service) mutate(ctx context.Context, tx pgx.Tx, q *store.Queries, actor audit.Actor, run store.PeopleImportRun, rec Record, m Mapping, teams []teamResolution, expected *previewState) (string, previewState, error) {
	v := values(rec, m)
	id, err := s.match(ctx, q, run.Source, v)
	if err != nil {
		return "conflict", previewState{}, err
	}
	if id != uuid.Nil {
		if _, err = q.GetPersonForUpdate(ctx, id); err != nil {
			return "conflict", previewState{}, err
		}
		// Matching identifiers can change while waiting for a directory edit.
		matched, matchErr := s.match(ctx, q, run.Source, v)
		if matchErr != nil {
			return "conflict", previewState{}, matchErr
		}
		if matched != id {
			return "conflict", previewState{}, errConflict
		}
	}
	// Lock field definitions before taking the version, including their edit,
	// sensitivity and archive settings. The profile service uses these locks too.
	if _, err = q.ListActiveProfileFieldsForShare(ctx); err != nil {
		return "conflict", previewState{}, err
	}
	before, err := snapshot(ctx, q, id)
	if err != nil {
		return "conflict", previewState{}, err
	}
	state := previewState{ID: id, Version: before, Teams: teams}
	if manager := v["manager_email"]; manager != "" {
		people, e := q.FindPeopleImportWorkEmail(ctx, manager)
		if e != nil {
			return "conflict", state, e
		}
		if len(people) != 1 {
			return "conflict", state, errConflict
		}
		state.ManagerID = people[0].ID
	}
	if expected != nil && (expected.ID != id || expected.ManagerID != state.ManagerID || !bytes.Equal(expected.Version, before) || !sameTeams(expected.Teams, teams)) {
		return "conflict", state, errStale
	}
	in := identity.ImportedPerson{ID: id, Username: v["username"], WorkEmail: v["work_email"], ContactEmail: v["contact_email"], DisplayName: v["display_name"], Title: v["title"], Role: authz.RoleKey(v["role"]), CreateTeams: m.CreateTeams, SendInvitation: m.SendInvitations}
	if state.ManagerID != uuid.Nil {
		in.ManagerID = &state.ManagerID
	}
	in.Teams = teamNames(v["teams"])
	if len(in.Teams) > 100 {
		return "conflict", state, identity.ErrInvalid
	}
	person, err := s.identity.ImportPersonTx(ctx, tx, q, actor, in)
	if err != nil {
		return "conflict", state, err
	}
	changes := []profile.Change{}
	for _, c := range m.Columns {
		if strings.HasPrefix(c.Target, "profile.") && v[c.Target] != "" {
			value := v[c.Target]
			changes = append(changes, profile.Change{Key: strings.TrimPrefix(c.Target, "profile."), Value: &value})
		}
	}
	if len(changes) > 0 {
		sensitive, e := canReadSensitive(ctx, q, run.CreatedBy)
		if e != nil {
			return "conflict", state, e
		}
		viewer := profile.Viewer{PersonID: run.CreatedBy, ManagePeople: true, ReadSensitive: sensitive}
		if _, err = s.profile.UpdateProfileTx(ctx, q, actor, viewer, person.ID, changes); err != nil {
			return "conflict", state, err
		}
	}
	if external := v["external_id"]; external != "" {
		if err = q.PutPeopleImportExternalID(ctx, store.PutPeopleImportExternalIDParams{Source: run.Source, ExternalHash: s.externalHash(external), PersonID: person.ID}); err != nil {
			return "conflict", state, err
		}
	}
	if id == uuid.Nil {
		return "create", state, nil
	}
	after, err := snapshot(ctx, q, id)
	if err != nil {
		return "conflict", state, err
	}
	if bytes.Equal(before, after) {
		return "skip", state, nil
	}
	return "update", state, nil
}

func duplicates(records []Record, m Mapping) map[int]bool {
	index := map[string][]int{}
	bad := map[int]bool{}
	for _, rec := range records {
		if rec.Error != "" || slices.Contains(m.SkipRows, rec.Number) {
			continue
		}
		v := values(rec, m)
		inRow := map[string]bool{}
		for _, key := range []string{"work_email", "contact_email", "external_id"} {
			if val := v[key]; val != "" {
				kind := key
				if key != "external_id" {
					val = strings.ToLower(val)
					kind = "email"
				}
				k := kind + ":" + val
				if !inRow[k] {
					index[k] = append(index[k], rec.Number)
					inRow[k] = true
				}
			}
		}
	}
	for _, rows := range index {
		if len(rows) > 1 {
			for _, n := range rows {
				bad[n] = true
			}
		}
	}
	return bad
}

// Preview saves the mapping and decisions after rolling back each simulated
// domain mutation. Fixed errors allow correcting rows without leaking values.
func (s *Service) Preview(ctx context.Context, actor audit.Actor, id uuid.UUID, m Mapping) (Run, error) {
	return s.process(ctx, actor, id, &m, false)
}

// Apply is idempotent for a run; per-row savepoints isolate all mutation/audit errors.
func (s *Service) Apply(ctx context.Context, actor audit.Actor, id uuid.UUID) (Run, error) {
	return s.process(ctx, actor, id, nil, true)
}
func (s *Service) process(ctx context.Context, actor audit.Actor, id uuid.UUID, mapping *Mapping, apply bool) (Run, error) {
	var out Run
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := identity.LockImportTx(ctx, q); err != nil {
			return err
		}
		caller, err := authorized(ctx, q, actor)
		if err != nil {
			return err
		}
		run, err := q.LockPeopleImportRun(ctx, store.LockPeopleImportRunParams{ID: id, CreatedBy: caller})
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if run.State == "applied" {
			if !apply {
				return ErrState
			}
			out, err = s.view(ctx, q, run)
			return err
		}
		m := emptyMapping()
		if apply {
			if run.State != "previewed" {
				return ErrState
			}
			if err = json.Unmarshal(run.Mapping, &m); err != nil {
				return err
			}
		} else {
			m = *mapping
		}
		if m.SkipRows == nil {
			m.SkipRows = []int{}
		}
		if err = s.validateMapping(ctx, q, run, m); err != nil {
			return err
		}
		rows, err := q.ListPeopleImportRows(ctx, id)
		if err != nil {
			return err
		}
		for _, n := range m.SkipRows {
			if !slices.ContainsFunc(rows, func(r store.PeopleImportRow) bool { return int(r.RowNumber) == n }) {
				return ErrInvalid
			}
		}
		records := make([]Record, len(rows))
		for i, r := range rows {
			if err = s.open(id, int(r.RowNumber), "row", r.PayloadSealed, &records[i]); err != nil {
				return err
			}
		}
		duplicate := duplicates(records, m)
		matchedRows := map[uuid.UUID][]int{}
		for _, rec := range records {
			if rec.Error != "" || slices.Contains(m.SkipRows, rec.Number) {
				continue
			}
			matched, matchErr := s.match(ctx, q, run.Source, values(rec, m))
			if matchErr != nil && !errors.Is(matchErr, errConflict) {
				return matchErr
			}
			if matchErr == nil && matched != uuid.Nil {
				matchedRows[matched] = append(matchedRows[matched], rec.Number)
			}
		}
		for _, matches := range matchedRows {
			if len(matches) > 1 {
				for _, number := range matches {
					duplicate[number] = true
				}
			}
		}
		teams, err := frozenTeamResolutions(ctx, q, records, m, duplicate)
		if err != nil {
			return err
		}
		for i, row := range rows {
			rec := records[i]
			decision, code := "skip", rec.Error
			var state previewState
			skip := slices.Contains(m.SkipRows, rec.Number)
			if duplicate[rec.Number] {
				code = "duplicate_identity"
			}
			if code != "" {
				decision = "conflict"
			}
			if apply && row.Decision == "conflict" {
				decision, code = row.Decision, row.ErrorCode
			}
			if code == "" && !skip && (!apply || row.Decision != "conflict") {
				var expected *previewState
				if apply {
					if err = s.open(id, rec.Number, "preview", row.PreviewSealed, &state); err != nil {
						return err
					}
					expected = &state
				}
				nested, err := tx.Begin(ctx)
				if err != nil {
					return err
				}
				decision, state, err = s.mutate(ctx, nested, store.New(nested), actor, run, rec, m, teams[rec.Number], expected)
				if err != nil {
					code = errorCode(err)
					decision = "conflict"
				}
				if !apply || err != nil {
					if rollbackErr := nested.Rollback(ctx); rollbackErr != nil {
						return rollbackErr
					}
				} else if err = nested.Commit(ctx); err != nil {
					return err
				}
			}
			if skip {
				decision, code = "skip", ""
			}
			if apply {
				var result string
				switch decision {
				case "create":
					result = "created"
				case "update":
					result = "updated"
				case "skip":
					result = "skipped"
				default:
					result = "failed"
				}
				if err = q.ResultPeopleImportRow(ctx, store.ResultPeopleImportRowParams{RunID: id, RowNumber: row.RowNumber, Result: result, ErrorCode: code}); err != nil {
					return err
				}
				if err = record(ctx, q, actor, id, "import.row_applied", map[string]any{"row": rec.Number, "decision": decision, "result": result, "error": code}); err != nil {
					return err
				}
			} else {
				sealed, err := s.seal(id, rec.Number, "preview", state)
				if err != nil {
					return err
				}
				if err = q.PreviewPeopleImportRow(ctx, store.PreviewPeopleImportRowParams{RunID: id, RowNumber: row.RowNumber, Decision: decision, ErrorCode: code, PreviewSealed: sealed}); err != nil {
					return err
				}
			}
		}
		if apply {
			if err = q.FinishPeopleImport(ctx, id); err != nil {
				return err
			}
			run.State = "applied"
			if err = record(ctx, q, actor, id, "import.applied", map[string]any{"rows": len(rows)}); err != nil {
				return err
			}
		} else {
			b, err := json.Marshal(m)
			if err != nil {
				return err
			}
			if err = q.SetPeopleImportMapping(ctx, store.SetPeopleImportMappingParams{ID: id, Mapping: b}); err != nil {
				return err
			}
			run.Mapping = b
			run.State = "previewed"
			if err = record(ctx, q, actor, id, "import.previewed", map[string]any{"rows": len(rows)}); err != nil {
				return err
			}
		}
		out, err = s.view(ctx, q, run)
		return err
	})
	return out, err
}
func errorCode(err error) string {
	switch {
	case errors.Is(err, errConflict):
		return "identity_conflict"
	case errors.Is(err, errStale):
		return "stale_preview"
	case errors.Is(err, identity.ErrForbidden), errors.Is(err, profile.ErrForbidden):
		return "permission_denied"
	case errors.Is(err, identity.ErrImportDelivery):
		return "invitation_delivery_unavailable"
	case errors.Is(err, identity.ErrInvalid), errors.Is(err, profile.ErrInvalid):
		return "invalid_fields"
	case errors.Is(err, identity.ErrUsernameTaken), errors.Is(err, identity.ErrEmailTaken):
		return "identity_conflict"
	case errors.Is(err, identity.ErrNotFound), errors.Is(err, profile.ErrNotFound):
		return "reference_not_found"
	case errors.Is(err, identity.ErrTeamArchived), errors.Is(err, identity.ErrPersonOffboarded):
		return "inactive_reference"
	default:
		return "row_failed"
	}
}

// ErrorsCSV exports no source data, file name, headers, tokens or field values.
func ErrorsCSV(run Run) string {
	var b strings.Builder
	w := csv.NewWriter(&b)
	_ = w.Write([]string{"row", "decision", "error", "result"})
	for _, r := range run.Rows {
		if r.Error != "" {
			_ = w.Write([]string{strconv.Itoa(r.Row), SafeCSVCell(r.Decision), SafeCSVCell(r.Error), SafeCSVCell(r.Result)})
		}
	}
	w.Flush()
	return b.String()
}

// Template contains column keys only, avoiding a fabricated example person.
func Template() string { return strings.Join(baseTargets, ",") + "\n" }

func canReadSensitive(ctx context.Context, q *store.Queries, id uuid.UUID) (bool, error) {
	permissions, err := q.ListPersonPermissions(ctx, id)
	return authz.PrincipalFrom(ctx).Has(authz.PeopleSensitiveRead) && slices.Contains(permissions, string(authz.PeopleSensitiveRead)), err
}
