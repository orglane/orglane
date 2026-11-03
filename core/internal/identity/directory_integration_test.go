//go:build integration

package identity_test

import (
	"bytes"
	"errors"
	"fmt"
	"image/png"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/orglane/orglane/core/internal/store"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/authz"
	"github.com/orglane/orglane/core/internal/identity"
)

func TestDirectorySearchFiltersAndPagination(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	// A real, realistically sized directory, not a mocked repository.
	_, err := pool.Exec(ctx, `INSERT INTO people (username,work_email,display_name,title,status)
      SELECT 'person.' || n, 'person.' || n || '@example.test', 'Person ' || lpad(n::text,4,'0'),
             CASE WHEN n % 2 = 0 THEN 'Robotics Engineer' ELSE 'Product Designer' END, 'active'
      FROM generate_series(1,1200) n`)
	if err != nil {
		t.Fatal(err)
	}
	// Include real role and team relationships for every generated person.
	if _, err := pool.Exec(ctx, `INSERT INTO person_roles (person_id,role_id)
      SELECT p.id,r.id FROM people p CROSS JOIN roles r WHERE r.key='member'`); err != nil {
		t.Fatal(err)
	}
	ann := createPerson(t, svc, "ann", identity.StatusActive)
	title := "100%_Reliable"
	if _, err := svc.UpdatePerson(ctx, admin, ann.ID, identity.PersonUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	if err := svc.AssignRole(ctx, admin, ann.ID, authz.RoleMember); err != nil {
		t.Fatal(err)
	}
	team, err := svc.CreateTeam(ctx, admin, identity.NewTeam{Slug: "platform", Name: "Platform"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO team_memberships (team_id,person_id,role)
      SELECT $1,id,'member' FROM people WHERE id<>$2`, team.ID, ann.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetTeamMember(ctx, admin, team.ID, ann.ID, identity.TeamRoleMember); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{"": 1201, "Robotics": 600, "person.101@": 1, "Person 0101": 1, "ann": 1, "%_": 1, "' OR 1=1 --": 0} {
		t.Run(query, func(t *testing.T) {
			count := 0
			var cursor *uuid.UUID
			seen := map[uuid.UUID]bool{}
			for {
				page, err := svc.DirectoryPage(ctx, identity.DirectoryOptions{Search: query, Limit: 73, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Items) > 73 {
					t.Fatal("unbounded page")
				}
				for _, row := range page.Items {
					if seen[row.Person.ID] {
						t.Fatal("duplicate person across pages")
					}
					seen[row.Person.ID] = true
					count++
				}
				if page.NextCursor == nil {
					break
				}
				cursor = page.NextCursor
			}
			if count != want {
				t.Fatalf("count=%d want %d", count, want)
			}
		})
	}
	started := time.Now()
	page, err := svc.DirectoryPage(ctx, identity.DirectoryOptions{Search: "ann", TeamID: &team.ID, Role: "member", Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Person.ID != ann.ID {
		t.Fatalf("combined filters: %+v %v", page, err)
	}
	t.Logf("1201-person combined directory query: %s", time.Since(started))
	page, err = svc.DirectoryPage(ctx, identity.DirectoryOptions{TeamID: &team.ID, Role: "owner", Limit: 10})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("role filter: %+v %v", page, err)
	}
	if _, err := svc.DirectoryPage(ctx, identity.DirectoryOptions{Limit: 101}); err == nil {
		t.Fatal("accepted unbounded limit")
	}
	var since time.Time
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&since); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveTeamMember(ctx, admin, team.ID, ann.ID); err != nil {
		t.Fatal(err)
	}
	page, err = svc.DirectoryPage(ctx, identity.DirectoryOptions{UpdatedSince: &since, Limit: 10})
	if err != nil || len(page.Items) != 1 || len(page.Items[0].Teams) != 0 {
		t.Fatalf("membership delta: %+v %v", page, err)
	}
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&since); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetPersonStatus(ctx, admin, ann.ID, identity.StatusOffboarded); err != nil {
		t.Fatal(err)
	}
	page, err = svc.DirectoryPage(ctx, identity.DirectoryOptions{UpdatedSince: &since, Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Person.Status != "offboarded" {
		t.Fatalf("tombstone delta: %+v %v", page, err)
	}
	if _, err := svc.DirectoryPage(ctx, identity.DirectoryOptions{Search: "ann", UpdatedSince: &since, Limit: 10}); err == nil {
		t.Fatal("accepted filtered delta")
	}
}

func TestManagerCyclesAndConcurrentChanges(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	a := createPerson(t, svc, "anna", identity.StatusActive)
	b := createPerson(t, svc, "ben", identity.StatusActive)
	c := createPerson(t, svc, "chris", identity.StatusActive)
	before := lastAuditID(t, pool)
	if _, err := svc.UpdatePerson(ctx, admin, a.ID, identity.PersonUpdate{ManagerID: &b.ID}); err != nil {
		t.Fatal(err)
	}
	entries := auditLog(t, pool, before)
	if len(entries) != 1 || entries[0].Action != string(audit.PersonUpdated) || string(entries[0].Metadata) != `{"fields": ["manager_id"]}` {
		t.Fatalf("manager audit: %+v", entries)
	}
	if _, err := svc.UpdatePerson(ctx, admin, b.ID, identity.PersonUpdate{ManagerID: &c.ID}); err != nil {
		t.Fatal(err)
	}
	for name, manager := range map[string]uuid.UUID{"self": c.ID, "ancestor": a.ID, "missing": uuid.New()} {
		t.Run(name, func(t *testing.T) {
			before := lastAuditID(t, pool)
			if _, err := svc.UpdatePerson(ctx, admin, c.ID, identity.PersonUpdate{ManagerID: &manager}); err == nil {
				t.Fatal("invalid reporting line accepted")
			}
			if len(auditLog(t, pool, before)) != 0 {
				t.Fatal("rejected manager change audited")
			}
		})
	}
	none := uuid.Nil
	if _, err := svc.UpdatePerson(ctx, admin, a.ID, identity.PersonUpdate{ManagerID: &none}); err != nil {
		t.Fatal(err)
	}
	for n := range 8 {
		x := createPerson(t, svc, fmt.Sprintf("race.x%d", n), identity.StatusActive)
		y := createPerson(t, svc, fmt.Sprintf("race.y%d", n), identity.StatusActive)
		var wg sync.WaitGroup
		start := make(chan struct{})
		result := make(chan error, 2)
		for _, pair := range [][2]uuid.UUID{{x.ID, y.ID}, {y.ID, x.ID}} {
			wg.Go(func() {
				<-start
				_, err := svc.UpdatePerson(ctx, admin, pair[0], identity.PersonUpdate{ManagerID: &pair[1]})
				result <- err
			})
		}
		close(start)
		wg.Wait()
		close(result)
		successes := 0
		for err := range result {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("concurrent cycle had %d successful writes", successes)
		}
	}
}

func TestUploadedRasterStoredSanitizedAndInvalidatesDirectory(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	person := createPerson(t, svc, "avatar.person", identity.StatusActive)
	before := lastAuditID(t, pool)
	original, err := svc.DirectoryPerson(ctx, person.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.UploadAvatar(ctx, admin, person.ID, "image/jpeg", rasterJPEG(t)); err != nil {
		t.Fatal(err)
	}
	stored, err := svc.GetAvatar(ctx, person.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.ContentType != "image/png" || bytes.Contains(stored.Data, []byte("Exif")) || bytes.Contains(stored.Data, []byte("GPS secret")) {
		t.Fatal("stored EXIF or source JPEG")
	}
	decoded, err := png.Decode(bytes.NewReader(stored.Data))
	if err != nil || decoded.Bounds().Dx() > 512 || decoded.Bounds().Dy() > 512 || len(stored.Data) > 256*1024 {
		t.Fatalf("unsafe stored raster: %v", err)
	}
	after, err := svc.DirectoryPerson(ctx, person.ID)
	if err != nil || !after.HasAvatar || !after.Person.UpdatedAt.After(original.Person.UpdatedAt) {
		t.Fatalf("directory not invalidated: %+v %v", after, err)
	}
	if len(auditLog(t, pool, before)) != 1 {
		t.Fatal("avatar mutation not audited once")
	}
	for mime, data := range map[string][]byte{"image/png": make([]byte, 5*1024*1024+1), "image/svg+xml": []byte(safeSVG), "image/jpeg": []byte("not an image")} {
		if err := svc.UploadAvatar(ctx, admin, person.ID, mime, data); err == nil {
			t.Fatalf("accepted %s", mime)
		}
	}
	unchanged, err := svc.GetAvatar(ctx, person.ID)
	if err != nil || !bytes.Equal(unchanged.Data, stored.Data) || len(auditLog(t, pool, before)) != 1 {
		t.Fatal("rejected input changed avatar or audit")
	}
}

func TestUpdatePersonTxOuterRollbackAndNoOp(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	person, err := svc.CreatePerson(ctx, admin, identity.NewPerson{Username: "tx.subject", WorkEmail: "tx.subject@acme.test", DisplayName: "Subject", ContactEmail: "private@personal.test", Status: identity.StatusActive})
	if err != nil {
		t.Fatal(err)
	}
	manager := createPerson(t, svc, "tx.manager", identity.StatusActive)
	before := lastAuditID(t, pool)
	name, title := "  Updated Name  ", "  Engineer  "
	change := identity.PersonUpdate{DisplayName: &name, Title: &title, ManagerID: &manager.ID}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := store.New(tx)
	if err := identity.AcquireReportingLock(ctx, q); err != nil {
		t.Fatal(err)
	}
	// A trusted caller may already hold this person lock after reporting.
	if _, err := q.GetPersonForUpdate(ctx, person.ID); err != nil {
		t.Fatal(err)
	}
	result, err := svc.UpdatePersonTx(ctx, q, admin, person.ID, change)
	if err != nil || result.DisplayName != "Updated Name" || result.Title == nil || *result.Title != "Engineer" || result.ManagerID == nil || *result.ManagerID != manager.ID {
		t.Fatalf("Tx update: %+v %v", result, err)
	}
	outside, err := svc.GetPerson(ctx, person.ID)
	if err != nil || outside.DisplayName != person.DisplayName || outside.ManagerID != nil || len(auditLog(t, pool, before)) != 0 {
		t.Fatal("Tx helper committed independently")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := svc.GetPerson(ctx, person.ID)
	if err != nil || after.DisplayName != person.DisplayName || after.ManagerID != nil || !after.UpdatedAt.Equal(person.UpdatedAt) || !after.DirectoryUpdatedAt.Equal(person.DirectoryUpdatedAt) || len(auditLog(t, pool, before)) != 0 {
		t.Fatal("outer rollback lost fields/timestamp/audit atomicity")
	}
	err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		q := store.New(tx)
		if err := identity.AcquireReportingLock(ctx, q); err != nil {
			return err
		}
		first, err := svc.UpdatePersonTx(ctx, q, admin, person.ID, change)
		if err != nil {
			return err
		}
		same, err := svc.UpdatePersonTx(ctx, q, admin, person.ID, change)
		if err != nil {
			return err
		}
		if !same.UpdatedAt.Equal(first.UpdatedAt) || !same.DirectoryUpdatedAt.Equal(first.DirectoryUpdatedAt) {
			t.Error("no-op changed timestamp")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(auditLog(t, pool, before)) != 1 {
		t.Fatal("no-op Tx call wrote another audit")
	}
	committed, err := svc.GetPerson(ctx, person.ID)
	if err != nil || committed.ContactEmail == nil || *committed.ContactEmail != "private@personal.test" {
		t.Fatal("base Tx update changed private contact email")
	}
}

func TestUpdatePersonTxConcurrentManagerCycle(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	a := createPerson(t, svc, "tx.a", identity.StatusActive)
	b := createPerson(t, svc, "tx.b", identity.StatusActive)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, pair := range [][2]uuid.UUID{{a.ID, b.ID}, {b.ID, a.ID}} {
		wg.Go(func() {
			<-start
			results <- pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				q := store.New(tx)
				if err := identity.AcquireReportingLock(ctx, q); err != nil {
					return err
				}
				if _, err := q.GetPersonForUpdate(ctx, pair[0]); err != nil {
					return err
				}
				_, err := svc.UpdatePersonTx(ctx, q, admin, pair[0], identity.PersonUpdate{ManagerID: &pair[1]})
				return err
			})
		})
	}
	close(start)
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		} else if !errors.Is(err, identity.ErrInvalid) {
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if successes != 1 {
		t.Fatalf("opposite Tx updates committed %d times", successes)
	}
}

func TestPrivateContactUpdateDoesNotEnterDirectoryDelta(t *testing.T) {
	svc, pool := newService(t)
	ctx := t.Context()
	p := createPerson(t, svc, "private.contact", identity.StatusActive)
	original, err := svc.DirectoryPerson(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	var since time.Time
	if err := pool.QueryRow(ctx, "SELECT clock_timestamp()").Scan(&since); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE people SET contact_email='private@personal.test',updated_at=clock_timestamp() WHERE id=$1`, p.ID); err != nil {
		t.Fatal(err)
	}
	projection, err := svc.DirectoryPerson(ctx, p.ID)
	if err != nil || projection.Person.ContactEmail != nil || !projection.Person.UpdatedAt.Equal(original.Person.UpdatedAt) {
		t.Fatal("private contact or clock escaped public projection")
	}
	page, err := svc.DirectoryPage(ctx, identity.DirectoryOptions{UpdatedSince: &since, Limit: 10})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("private contact change entered public delta: %+v %v", page, err)
	}
	title := "Public change"
	if _, err := svc.UpdatePerson(ctx, admin, p.ID, identity.PersonUpdate{Title: &title}); err != nil {
		t.Fatal(err)
	}
	page, err = svc.DirectoryPage(ctx, identity.DirectoryOptions{UpdatedSince: &since, Limit: 10})
	if err != nil || len(page.Items) != 1 || page.Items[0].Person.ID != p.ID {
		t.Fatalf("public change missing from delta: %+v %v", page, err)
	}
}
