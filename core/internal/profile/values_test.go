package profile

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalize(t *testing.T) {
	sel := Field{Key: "office", Type: TypeSelect, Options: []Option{{Key: "berlin", Label: "Berlin"}, {Key: "lisbon", Label: "Lisbon"}}}
	handle := func(k HandleKind) Field { return Field{Key: string(k), Type: TypeHandle, HandleKind: k} }
	tests := []struct {
		name  string
		field Field
		in    string
		want  string // "" with ok: cleared
		ok    bool
	}{
		{"text", Field{Type: TypeText}, "  she/her ", "she/her", true},
		{"text empty clears", Field{Type: TypeText}, "   ", "", true},
		{"text too long", Field{Type: TypeText}, strings.Repeat("a", 201), "", false},
		{"text max", Field{Type: TypeText}, strings.Repeat("ä", 200), strings.Repeat("ä", 200), true},
		{"text newline", Field{Type: TypeText}, "a\nb", "", false},
		{"text control", Field{Type: TypeText}, "a\x07b", "", false},
		{"text line separator", Field{Type: TypeText}, "a b", "", false},
		{"text NUL", Field{Type: TypeText}, "a\x00", "", false},
		{"text invalid utf8", Field{Type: TypeText}, "a\xffb", "", false},
		{"long text newlines", Field{Type: TypeLongText}, "Line 1\r\nLine 2\n\tindent", "Line 1\nLine 2\n\tindent", true},
		{"long text too long", Field{Type: TypeLongText}, strings.Repeat("a", 2001), "", false},
		{"long text control", Field{Type: TypeLongText}, "a\x1bb", "", false},
		{"date", Field{Type: TypeDate}, "1990-03-14", "1990-03-14", true},
		{"date leap day", Field{Type: TypeDate}, "2000-02-29", "2000-02-29", true},
		{"date invalid day", Field{Type: TypeDate}, "2001-02-29", "", false},
		{"date format", Field{Type: TypeDate}, "14.03.1990", "", false},
		{"date with time", Field{Type: TypeDate}, "1990-03-14T00:00:00Z", "", false},
		{"date too early", Field{Type: TypeDate}, "1899-12-31", "", false},
		{"date too late", Field{Type: TypeDate}, "2101-01-01", "", false},
		{"select", sel, "lisbon", "lisbon", true},
		{"select label is not a key", sel, "Lisbon", "", false},
		{"select unknown", sel, "paris", "", false},
		{"phone", Field{Type: TypePhone}, "+44 20 7946-0958", "+442079460958", true},
		{"phone parentheses and dots", Field{Type: TypePhone}, "+1 (555) 010.4477", "+15550104477", true},
		{"phone 00 prefix", Field{Type: TypePhone}, "0049 30 1234567", "+49301234567", true},
		{"phone national", Field{Type: TypePhone}, "030 1234567", "", false},
		{"phone letters", Field{Type: TypePhone}, "+1 555 CALL ME", "", false},
		{"phone too short", Field{Type: TypePhone}, "+12345", "", false},
		{"phone too long", Field{Type: TypePhone}, "+1234567890123456", "", false},
		{"url", Field{Type: TypeURL}, "https://example.com/me", "https://example.com/me", true},
		{"url http", Field{Type: TypeURL}, "http://example.com", "http://example.com", true},
		{"url javascript", Field{Type: TypeURL}, "javascript:alert(1)", "", false},
		{"url data", Field{Type: TypeURL}, "data:text/html,hi", "", false},
		{"url relative", Field{Type: TypeURL}, "/people", "", false},
		{"url without host", Field{Type: TypeURL}, "https:///x", "", false},
		{"url credentials", Field{Type: TypeURL}, "https://user:pw@example.com", "", false},
		{"url spaces", Field{Type: TypeURL}, "https://example.com/a b", "", false},
		{"telegram", handle(HandleTelegram), "@maya_okafor", "maya_okafor", true},
		{"telegram too short", handle(HandleTelegram), "maya", "", false},
		{"telegram starts with digit", handle(HandleTelegram), "1maya_ok", "", false},
		{"github", handle(HandleGitHub), "maya-okafor", "maya-okafor", true},
		{"github double dash", handle(HandleGitHub), "maya--okafor", "", false},
		{"github trailing dash", handle(HandleGitHub), "maya-", "", false},
		{"github too long", handle(HandleGitHub), strings.Repeat("a", 40), "", false},
		{"gitlab", handle(HandleGitLab), "maya.okafor", "maya.okafor", true},
		{"x", handle(HandleX), "@maya_ok", "maya_ok", true},
		{"x too long", handle(HandleX), "abcdefghijklmnop", "", false},
		{"mastodon", handle(HandleMastodon), "@maya@Social.Example", "maya@social.example", true},
		{"mastodon without instance", handle(HandleMastodon), "maya", "", false},
		{"handle with url", handle(HandleGitHub), "https://github.com/maya", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.field.normalize(tc.in)
			if tc.ok {
				if err != nil || got != tc.want {
					t.Fatalf("normalize(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
				}
				return
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("normalize(%q) = %q, %v; want ErrInvalid", tc.in, got, err)
			}
		})
	}
}

func TestMaskYear(t *testing.T) {
	if got := maskYear("1990-03-14"); got != "--03-14" {
		t.Fatalf("maskYear = %q", got)
	}
}

func TestDeriveKey(t *testing.T) {
	taken := map[string]bool{"office": true}
	tests := map[string]string{
		"Office":                "office_2",
		"Office location":       "office_location",
		"  Équipe / Région  ":   "quipe_r_gion",
		"2nd phone":             "field_2nd_phone",
		"Телефон":               "field",
		"Birthday":              "birthday_2", // reserved for the library
		strings.Repeat("a", 80): strings.Repeat("a", 63),
	}
	for label, want := range tests {
		if got := deriveKey(label, taken); got != want {
			t.Errorf("deriveKey(%q) = %q, want %q", label, got, want)
		}
		if got := deriveKey(label, taken); !keyPattern.MatchString(got) {
			t.Errorf("deriveKey(%q) = %q is not a valid key", label, got)
		}
	}
	long := strings.Repeat("b", 63)
	if got := deriveKey(long, map[string]bool{long: true}); got != strings.Repeat("b", 61)+"_2" {
		t.Errorf("deriveKey(long, taken) = %q", got)
	}
}

func TestNormalizeOptions(t *testing.T) {
	got, err := normalizeOptions(TypeSelect, []Option{{Label: " Berlin "}, {Label: "São Paulo"}, {Key: "nyc", Label: "New York"}, {Label: "Berlin!"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []Option{{Key: "berlin", Label: "Berlin"}, {Key: "s_o_paulo", Label: "São Paulo"}, {Key: "nyc", Label: "New York"}, {Key: "berlin_2", Label: "Berlin!"}}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("option %d = %v, want %v", i, got[i], want[i])
		}
	}
	for name, in := range map[string][]Option{
		"none":          {},
		"empty label":   {{Label: " "}},
		"duplicate key": {{Key: "a", Label: "A"}, {Key: "a", Label: "B"}},
		"bad key":       {{Key: "A B", Label: "A"}},
	} {
		if _, err := normalizeOptions(TypeSelect, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := normalizeOptions(TypeText, []Option{{Label: "A"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("options on a text field: err = %v", err)
	}
}

func TestLibrary(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range Library() {
		if seen[e.Key] {
			t.Errorf("duplicate library key %q", e.Key)
		}
		seen[e.Key] = true
		f := Field{Key: e.Key, Library: true, Label: e.Label, Type: e.Type, HandleKind: e.HandleKind, Options: e.Options,
			Section: e.Section, Visibility: e.Visibility, EditableBy: e.EditableBy}
		if !keyPattern.MatchString(e.Key) {
			t.Errorf("%s: invalid key", e.Key)
		}
		if err := f.validateSettings(); err != nil {
			t.Errorf("%s: %v", e.Key, err)
		}
		if _, err := normalizeLabel(e.Label); err != nil {
			t.Errorf("%s: %v", e.Key, err)
		}
		if got, err := normalizeOptions(e.Type, e.Options); err != nil || len(got) != len(e.Options) {
			t.Errorf("%s: options: %v", e.Key, err)
		}
		if (e.Type == TypeHandle) != (e.HandleKind != "") {
			t.Errorf("%s: handle kind mismatch", e.Key)
		}
	}
	for _, key := range []string{"telegram", "phone", "birthday", "tshirt_size", "pronouns", "location", "start_date"} {
		if !seen[key] {
			t.Errorf("library lacks %q", key)
		}
	}
}
