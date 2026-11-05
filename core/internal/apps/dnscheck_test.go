package apps

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok {
		return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
	}
	if len(addrs) == 1 && addrs[0] == "timeout" {
		return nil, &net.DNSError{Err: "i/o timeout", Name: host, IsTimeout: true}
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		out = append(out, netip.MustParseAddr(a))
	}
	return out, nil
}

func TestCheckServiceHost(t *testing.T) {
	r := fakeResolver{
		"work.acme.com":      {"203.0.113.7", "2001:db8::7"},
		"git.acme.com":       {"2001:db8::7", "203.0.113.7", "203.0.113.7"}, // order and duplicates do not matter
		"mapped.acme.com":    {"::ffff:203.0.113.7", "2001:db8::7"},
		"v4only.acme.com":    {"203.0.113.7"},
		"elsewhere.acme.com": {"198.51.100.1", "2001:db8::7"},
		"empty.acme.com":     {},
		"slow.acme.com":      {"timeout"},
	}
	for host, want := range map[string]string{
		"git.acme.com":       "",
		"mapped.acme.com":    "",
		"v4only.acme.com":    CodeDNSMismatch,
		"elsewhere.acme.com": CodeDNSMismatch,
		"missing.acme.com":   CodeDNSUnresolved,
		"empty.acme.com":     CodeDNSUnresolved,
		"slow.acme.com":      CodeDNSLookupFailed,
	} {
		err := CheckServiceHost(t.Context(), r, "work.acme.com", host)
		var de *DNSError
		switch {
		case want == "" && err != nil:
			t.Errorf("%s: %v", host, err)
		case want != "" && (!errors.As(err, &de) || de.Code != want):
			t.Errorf("%s: error %v, want %s", host, err, want)
		}
	}
	err := CheckServiceHost(t.Context(), r, "elsewhere.acme.com", "git.acme.com")
	var de *DNSError
	if !errors.As(err, &de) || len(de.Addresses) != 2 || len(de.Expected) != 2 || de.Host != "git.acme.com" {
		t.Errorf("mismatch details: %+v", de)
	}
	if err := CheckServiceHost(t.Context(), r, "nowhere.acme.com", "git.acme.com"); !errors.As(err, &de) || de.Code != CodeDNSLookupFailed {
		t.Errorf("unresolvable workspace: %v", err)
	}
}
