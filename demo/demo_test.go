package demo_test

import (
	"bytes"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/orglane/orglane/demo"
)

func TestAcme(t *testing.T) {
	d, err := demo.Load(demo.Acme)
	if err != nil {
		t.Fatal(err)
	}
	if d.Name != demo.Acme || d.Version < 1 || d.Company.Name != "Acme Robotics" {
		t.Fatalf("dataset = %q v%d %q", d.Name, d.Version, d.Company.Name)
	}
	if len(d.People) < 12 {
		t.Fatalf("%d people, want at least 12", len(d.People))
	}
	roles := map[string]int{}
	for _, p := range d.People {
		roles[p.Role]++
		if p.Title == "" || p.Avatar == "" {
			t.Errorf("%s: every demo person needs a title and an avatar", p.Username)
		}
		if !strings.HasSuffix(p.WorkEmail, ".example") {
			t.Errorf("%s: work email %q must use a reserved .example domain", p.Username, p.WorkEmail)
		}
	}
	if roles[demo.RoleOwner] != 1 || roles[demo.RoleAdmin] != 2 || roles[demo.RoleMember] != len(d.People)-3 {
		t.Errorf("roles = %v, want 1 owner, 2 admins, the rest members", roles)
	}
	if owner := d.Owner(); owner.Username != d.DefaultUsername || owner.Role != demo.RoleOwner {
		t.Errorf("Owner() = %+v, default %q", owner, d.DefaultUsername)
	}
	// 4-6 teams with realistic memberships: every team has a lead and
	// members, and everyone belongs to at least one team.
	if len(d.Teams) < 4 || len(d.Teams) > 6 {
		t.Errorf("%d teams, want 4-6", len(d.Teams))
	}
	inTeam := map[string]bool{}
	for _, team := range d.Teams {
		if len(team.Leads) == 0 || len(team.Members) == 0 || team.Description == "" {
			t.Errorf("team %s: needs a description, a lead and members", team.Slug)
		}
		for _, u := range append(team.Leads, team.Members...) {
			inTeam[u] = true
		}
	}
	for _, p := range d.People {
		if !inTeam[p.Username] {
			t.Errorf("%s belongs to no team", p.Username)
		}
	}
}

// TestAvatarsAreGenerated fails when the committed SVG avatars differ from
// what `go generate ./demo` writes.
func TestAvatarsAreGenerated(t *testing.T) {
	d, err := demo.Load(demo.Acme)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range d.People {
		data, contentType, ok, err := d.Avatar(p)
		if err != nil || !ok || contentType != "image/svg+xml" {
			t.Fatalf("%s: Avatar() = %q, %v, %v", p.Username, contentType, ok, err)
		}
		if want := demo.InitialsAvatar(p.DisplayName, p.Username); !bytes.Equal(data, want) {
			t.Errorf("%s: %s is stale, run `go generate ./demo`", p.Username, p.Avatar)
		}
	}
}

func TestInitials(t *testing.T) {
	for name, want := range map[string]string{
		"Mateo Rodríguez":   "MR",
		"Liam O'Connor":     "LO",
		"Elif Yılmaz":       "EY",
		"Chen Wei":          "CW",
		"Madonna":           "M",
		"  ":                "?",
		"Anna van der Berg": "AB",
	} {
		if got := demo.Initials(name); got != want {
			t.Errorf("Initials(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestInitialsAvatarEscapes(t *testing.T) {
	svg := string(demo.InitialsAvatar(`<script>"&`, "x"))
	if strings.Contains(svg, "<script>") || !strings.Contains(svg, "&lt;script&gt;") {
		t.Fatalf("name is not escaped:\n%s", svg)
	}
}

func TestLoadRejectsInvalidDatasets(t *testing.T) {
	const meta = "version: 1\ncompany: {name: Acme, email_domain: acme.example}\ndefault_username: ann\n"
	person := func(username, email, role string) string {
		return "  - {username: " + username + ", display_name: X Y, work_email: " + email + ", title: T, role: " + role + "}\n"
	}
	owner := person("ann", "ann@acme.example", "owner")
	for name, tc := range map[string]struct {
		meta, people, teams, want string
	}{
		"team without slug":    {meta, owner, "  - {name: A, leads: [ann]}\n", "slug must be set"},
		"team slug upper case": {meta, owner, "  - {slug: Core, name: A}\n", "lower case"},
		"duplicate team":       {meta, owner, "  - {slug: a, name: A}\n  - {slug: a, name: B}\n", "duplicate slug"},
		"team without name":    {meta, owner, "  - {slug: a}\n", "name is required"},
		"unknown team member":  {meta, owner, "  - {slug: a, name: A, members: [zed]}\n", `"zed" is not a person`},
		"lead and member":      {meta, owner, "  - {slug: a, name: A, leads: [ann], members: [ann]}\n", "more than once"},
		"unknown team field":   {meta, owner, "  - {slug: a, name: A, owner: ann}\n", "owner"},
		"no owner":             {meta, person("ann", "ann@acme.example", "admin"), "", "exactly one person must be the owner"},
		"two owners":           {meta, person("ann", "ann@acme.example", "owner") + person("bob", "bob@acme.example", "owner"), "", "exactly one"},
		"duplicate user":       {meta, person("ann", "ann@acme.example", "owner") + person("ann", "ann2@acme.example", "member"), "", "duplicate username"},
		"duplicate email":      {meta, person("ann", "ann@acme.example", "owner") + person("bob", "ANN@acme.example", "member"), "", "duplicate work_email"},
		"foreign domain":       {meta, person("ann", "ann@evil.example", "owner"), "", "company domain"},
		"unknown role":         {meta, person("ann", "ann@acme.example", "owner") + person("bob", "bob@acme.example", "root"), "", "role must be"},
		"default missing":      {strings.Replace(meta, "ann", "zed", 1), person("ann", "ann@acme.example", "owner"), "", "default_username"},
		"unknown field":        {meta + "flavor: red\n", person("ann", "ann@acme.example", "owner"), "", "flavor"},
		"missing avatar":       {meta, "  - {username: ann, display_name: A B, work_email: ann@acme.example, role: owner, avatar: avatars/ann.svg}\n", "", "avatar"},
		"avatar traversal":     {meta, "  - {username: ann, display_name: A B, work_email: ann@acme.example, role: owner, avatar: ../x.svg}\n", "", "inside the dataset"},
	} {
		t.Run(name, func(t *testing.T) {
			fsys := fstest.MapFS{
				"dataset.yaml": {Data: []byte(tc.meta)},
				"people.yaml":  {Data: []byte("people:\n" + tc.people)},
			}
			if tc.teams != "" {
				fsys["teams.yaml"] = &fstest.MapFile{Data: []byte("teams:\n" + tc.teams)}
			}
			_, err := demo.LoadFS("test", fsys)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadFS() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
	if _, err := demo.Load("../core"); err == nil {
		t.Error("Load accepted a path outside the datasets")
	}
	if _, err := demo.Load("nope"); err == nil {
		t.Error("Load accepted an unknown dataset")
	}
}
