package apps

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"time"
)

// DNS check codes (docs/specs/providers.md "Managed instance lifecycle",
// step 1).
const (
	// CodeDNSUnresolved: the service host has no A or AAAA record (yet).
	CodeDNSUnresolved = "dns_unresolved"
	// CodeDNSMismatch: the service host resolves to other addresses than
	// the workspace host, so Traefik on this server would not get its
	// requests and Let's Encrypt could not issue its certificate.
	CodeDNSMismatch = "dns_mismatch"
	// CodeDNSLookupFailed: the workspace host could not be looked up, so
	// nothing can be compared.
	CodeDNSLookupFailed = "dns_lookup_failed"
)

// dnsTimeout bounds both lookups together.
const dnsTimeout = 10 * time.Second

// Resolver looks up the addresses of a host; *net.Resolver is one.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// DNSError is a failed DNS check. It names the host and, for a mismatch,
// the addresses on both sides: what the admin needs to fix the records.
type DNSError struct {
	Code      string
	Host      string
	Addresses []netip.Addr // the service host's addresses
	Expected  []netip.Addr // the workspace host's addresses
	Err       error
}

func (e *DNSError) Error() string {
	switch e.Code {
	case CodeDNSMismatch:
		return fmt.Sprintf("apps: %s resolves to %v, the workspace to %v", e.Host, e.Addresses, e.Expected)
	case CodeDNSUnresolved:
		return fmt.Sprintf("apps: %s does not resolve", e.Host)
	}
	return fmt.Sprintf("apps: DNS lookup of %s failed: %v", e.Host, e.Err)
}

func (e *DNSError) Unwrap() error { return e.Err }

// CheckServiceHost checks, in domain mode, that host resolves to exactly
// the addresses of the workspace host: both are served by this server's
// Traefik, and Let's Encrypt validates every address in DNS (as the
// installer checks the workspace host). Local mode needs no check: every
// *.localhost name is loopback.
func CheckServiceHost(ctx context.Context, r Resolver, workspaceHost, host string) error {
	ctx, cancel := context.WithTimeout(ctx, dnsTimeout)
	defer cancel()
	want, err := lookup(ctx, r, workspaceHost)
	if err != nil {
		return &DNSError{Code: CodeDNSLookupFailed, Host: workspaceHost, Err: err}
	}
	if len(want) == 0 {
		return &DNSError{Code: CodeDNSLookupFailed, Host: workspaceHost, Err: errors.New("no addresses")}
	}
	got, err := lookup(ctx, r, host)
	var dnsErr *net.DNSError
	switch {
	case err == nil && len(got) == 0, errors.As(err, &dnsErr) && dnsErr.IsNotFound:
		return &DNSError{Code: CodeDNSUnresolved, Host: host, Expected: want, Err: err}
	case err != nil:
		return &DNSError{Code: CodeDNSLookupFailed, Host: host, Err: err}
	}
	if !slices.Equal(got, want) {
		return &DNSError{Code: CodeDNSMismatch, Host: host, Addresses: got, Expected: want}
	}
	return nil
}

// lookup returns the sorted, unique addresses of a host.
func lookup(ctx context.Context, r Resolver, host string) ([]netip.Addr, error) {
	addrs, err := r.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	for i, a := range addrs {
		addrs[i] = a.Unmap()
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(addrs), nil
}
