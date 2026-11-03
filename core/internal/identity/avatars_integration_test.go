//go:build integration

package identity_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/orglane/orglane/core/internal/audit"
	"github.com/orglane/orglane/core/internal/identity"
)

func TestPersonTitle(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	p, err := svc.CreatePerson(ctx, admin, identity.NewPerson{
		Username: "ann", WorkEmail: "ann@acme.test", DisplayName: "Ann", Title: "  Robotics Engineer ",
	})
	if err != nil {
		t.Fatal(err)
	}
	if p.Title == nil || *p.Title != "Robotics Engineer" {
		t.Fatalf("title = %v, want trimmed", p.Title)
	}
	before := lastAuditID(t, pool)
	same := "Robotics Engineer"
	if _, err := svc.UpdatePerson(ctx, admin, p.ID, identity.PersonUpdate{Title: &same}); err != nil {
		t.Fatal(err)
	}
	if n := len(auditLog(t, pool, before)); n != 0 {
		t.Fatalf("an unchanged title wrote %d audit entries", n)
	}
	empty := ""
	if p, err = svc.UpdatePerson(ctx, admin, p.ID, identity.PersonUpdate{Title: &empty}); err != nil || p.Title != nil {
		t.Fatalf("clearing the title: %v, %v", p.Title, err)
	}
	entries := auditLog(t, pool, before)
	if len(entries) != 1 || entries[0].Action != string(audit.PersonUpdated) || string(entries[0].Metadata) != `{"fields": ["title"]}` {
		t.Fatalf("audit = %+v", entries)
	}
	tooLong := strings.Repeat("a", 201)
	if _, err := svc.UpdatePerson(ctx, admin, p.ID, identity.PersonUpdate{Title: &tooLong}); !errors.Is(err, identity.ErrInvalid) {
		t.Fatalf("201-character title: %v, want ErrInvalid", err)
	}
}

func TestAvatars(t *testing.T) {
	ctx := context.Background()
	svc, pool := newService(t)
	p := createPerson(t, svc, "ann", identity.StatusActive)

	if _, err := svc.GetAvatar(ctx, p.ID); !errors.Is(err, identity.ErrNotFound) {
		t.Fatalf("GetAvatar without avatar = %v, want ErrNotFound", err)
	}
	before := lastAuditID(t, pool)
	if err := svc.SetAvatar(ctx, admin, p.ID, identity.Avatar{ContentType: identity.AvatarSVG, Data: []byte(safeSVG)}); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetAvatar(ctx, p.ID)
	if err != nil || got.ContentType != identity.AvatarSVG || string(got.Data) != safeSVG {
		t.Fatalf("GetAvatar = %q, %v", got.ContentType, err)
	}
	entries := auditLog(t, pool, before)
	if len(entries) != 1 || entries[0].Action != string(audit.PersonAvatarSet) || *entries[0].TargetID != p.ID.String() {
		t.Fatalf("audit = %+v", entries)
	}

	// A malicious SVG is rejected on write: nothing is stored or audited.
	before = lastAuditID(t, pool)
	for _, evil := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(document.cookie)"><script>alert(1)</script></svg>`,
		// CSS escapes spelling url(): an external paint server reference.
		`<svg xmlns="http://www.w3.org/2000/svg"><rect fill="\75rl(https://evil.example/a.svg#g)"/></svg>`,
		`<svg xmlns="http://www.w3.org/2000/svg"><rect fill="u\72l(https://evil.example/a.svg#g)"/></svg>`,
	} {
		if err := svc.SetAvatar(ctx, admin, p.ID, identity.Avatar{ContentType: identity.AvatarSVG, Data: []byte(evil)}); !errors.Is(err, identity.ErrUnsafeAvatar) {
			t.Fatalf("SetAvatar(%s) = %v, want ErrUnsafeAvatar", evil, err)
		}
	}
	if got, _ := svc.GetAvatar(ctx, p.ID); string(got.Data) != safeSVG {
		t.Fatal("the malicious SVG replaced the avatar")
	}
	if n := len(auditLog(t, pool, before)); n != 0 {
		t.Fatalf("a rejected avatar wrote %d audit entries", n)
	}
	// The database enforces the content type allow-list on its own.
	if _, err := pool.Exec(ctx, `UPDATE person_avatars SET content_type = 'text/html' WHERE person_id = $1`, p.ID); err == nil {
		t.Fatal("the database accepted a text/html avatar")
	}

	before = lastAuditID(t, pool)
	if err := svc.RemoveAvatar(ctx, admin, p.ID); err != nil {
		t.Fatal(err)
	}
	if err := svc.RemoveAvatar(ctx, admin, p.ID); err != nil {
		t.Fatal(err)
	}
	entries = auditLog(t, pool, before)
	if len(entries) != 1 || entries[0].Action != string(audit.PersonAvatarRemoved) {
		t.Fatalf("audit = %+v", entries)
	}
}
