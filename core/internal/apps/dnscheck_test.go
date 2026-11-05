package apps

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
)

// fakeResolver answers names as listed, rooted ("git.acme.com.") or not:
// an unrooted entry stands for what a search domain would add.
type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	addrs, ok := f[host]
	if !ok && strings.HasSuffix(host, ".") {
		addrs, ok = f[strings.TrimSuffix(host, ".")]
		if _, unrooted := f["unrooted:"+strings.TrimSuffix(host, ".")]; unrooted {
			ok = false
		}
	}
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
	// Only the unrooted name resolves (a wildcard under a search domain):
	// the public name has no record.
	searched := fakeResolver{"work.acme.com": {"203.0.113.7"}, "git.acme.com": {"203.0.113.7"}, "unrooted:git.acme.com": nil}
	var asked []string
	if err := CheckServiceHost(t.Context(), recording{searched, &asked}, "work.acme.com", "git.acme.com"); !errors.As(err, new(*DNSError)) {
		t.Errorf("search-suffixed name accepted: %v", err)
	}
	for _, name := range asked {
		if !strings.HasSuffix(name, ".") || strings.HasSuffix(name, "..") {
			t.Errorf("looked up %q, want a rooted name", name)
		}
	}
	if err := CheckServiceHost(t.Context(), recording{r, &asked}, "work.acme.com.", "git.acme.com."); err != nil {
		t.Errorf("rooted input: %v", err)
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

type recording struct {
	r     fakeResolver
	asked *[]string
}

func (r recording) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	*r.asked = append(*r.asked, host)
	return r.r.LookupNetIP(ctx, network, host)
}

func TestNewDNSResolver(t *testing.T) {
	r := NewDNSResolver()
	if !r.PreferGo || !r.StrictErrors {
		t.Fatalf("resolver = %+v: partial results would pass", r)
	}
}
