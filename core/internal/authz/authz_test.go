package authz

import (
	"context"
	"errors"
	"net/http/httptest"
	"testing"
)

func TestKnown(t *testing.T) {
	if !Known(InstanceRead) {
		t.Errorf("Known(%q) = false, want true", InstanceRead)
	}
	for _, p := range []Permission{Public, "", "instance.write", "INSTANCE.READ"} {
		if Known(p) {
			t.Errorf("Known(%q) = true, want false", p)
		}
	}
}

func TestPrincipalHas(t *testing.T) {
	p := &Principal{Subject: "alice", Permissions: []Permission{InstanceRead}}
	if !p.Has(InstanceRead) {
		t.Error("Has(InstanceRead) = false, want true")
	}
	if p.Has("people.manage") {
		t.Error("Has(people.manage) = true, want false")
	}
	var nilPrincipal *Principal
	if nilPrincipal.Has(InstanceRead) {
		t.Error("nil principal Has() = true, want false")
	}
}

func TestDenyAll(t *testing.T) {
	p, err := DenyAll{}.Authenticate(httptest.NewRequestWithContext(t.Context(), "GET", "/", nil))
	if p != nil || !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("DenyAll.Authenticate() = %v, %v; want nil, ErrUnauthenticated", p, err)
	}
}

func TestPrincipalContext(t *testing.T) {
	if got := PrincipalFrom(context.Background()); got != nil {
		t.Fatalf("PrincipalFrom(empty) = %v, want nil", got)
	}
	p := &Principal{Subject: "alice"}
	if got := PrincipalFrom(WithPrincipal(context.Background(), p)); got != p {
		t.Fatalf("PrincipalFrom() = %v, want %v", got, p)
	}
}
