package audit

import (
	"errors"
	"net/netip"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestEntryParams(t *testing.T) {
	ip := netip.MustParseAddr("::ffff:203.0.113.7")
	p, err := Entry{
		Actor:    Actor{Kind: ActorPerson, ID: "p1", IP: ip, UserAgent: "curl/8"},
		Action:   PersonCreated,
		Target:   Target{Kind: TargetPerson, ID: "p2"},
		Metadata: map[string]any{"status": "invited"},
	}.params()
	if err != nil {
		t.Fatal(err)
	}
	if p.ActorKind != "person" || *p.ActorID != "p1" || p.Action != "person.created" ||
		*p.TargetKind != "person" || *p.TargetID != "p2" || string(p.Metadata) != `{"status":"invited"}` ||
		p.IP.String() != "203.0.113.7" || *p.UserAgent != "curl/8" {
		t.Fatalf("params() = %+v", p)
	}

	p, err = Entry{Actor: System, Action: RoleCreated}.params()
	if err != nil {
		t.Fatal(err)
	}
	if p.ActorID != nil || p.TargetKind != nil || p.TargetID != nil || p.IP != nil || p.UserAgent != nil || string(p.Metadata) != "{}" {
		t.Fatalf("params() for a system entry without target = %+v", p)
	}

	var nilMap map[string]any
	if p, err = (Entry{Actor: System, Action: RoleCreated, Metadata: nilMap}).params(); err != nil || string(p.Metadata) != "{}" {
		t.Fatalf("params() with a nil map = %s, %v; want {}", p.Metadata, err)
	}
}

func TestEntryParamsRejects(t *testing.T) {
	tests := map[string]Entry{
		"no actor kind":        {Action: PersonCreated},
		"unknown actor kind":   {Actor: Actor{Kind: "robot", ID: "r"}, Action: PersonCreated},
		"person without id":    {Actor: Actor{Kind: ActorPerson}, Action: PersonCreated},
		"module without id":    {Actor: Actor{Kind: ActorModule}, Action: PersonCreated},
		"malformed action":     {Actor: System, Action: "Person Created"},
		"action without area":  {Actor: System, Action: "created"},
		"target without id":    {Actor: System, Action: PersonCreated, Target: Target{Kind: TargetPerson}},
		"target without kind":  {Actor: System, Action: PersonCreated, Target: Target{ID: "x"}},
		"metadata not object":  {Actor: System, Action: PersonCreated, Metadata: []string{"a"}},
		"metadata unmarshable": {Actor: System, Action: PersonCreated, Metadata: map[string]any{"f": func() {}}},
	}
	for name, e := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := e.params(); !errors.Is(err, ErrInvalidEntry) {
				t.Fatalf("params() error = %v, want ErrInvalidEntry", err)
			}
		})
	}
}

func TestUserAgentSanitized(t *testing.T) {
	long := strings.Repeat("é", MaxUserAgentLength) // 2 bytes per character
	p, err := Entry{Actor: Actor{Kind: ActorSystem, UserAgent: long}, Action: PersonCreated}.params()
	if err != nil {
		t.Fatal(err)
	}
	if len(*p.UserAgent) > MaxUserAgentLength || !utf8.ValidString(*p.UserAgent) {
		t.Fatalf("user agent not truncated on a character boundary: %d bytes, valid=%v", len(*p.UserAgent), utf8.ValidString(*p.UserAgent))
	}

	p, err = Entry{Actor: Actor{Kind: ActorSystem, UserAgent: "a\x00b\xffc"}, Action: PersonCreated}.params()
	if err != nil {
		t.Fatal(err)
	}
	if *p.UserAgent != "ab�c" {
		t.Fatalf("user agent = %q, want NUL dropped and invalid UTF-8 replaced", *p.UserAgent)
	}
}
