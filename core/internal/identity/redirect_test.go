package identity

import "testing"

// Plain-http redirect URIs: literal loopback hosts always; the local-mode
// service domain and its subdomains only when configured, and nothing that
// merely looks like them.
func TestValidateRedirectURILocalDomain(t *testing.T) {
	const local = "orglane.localhost"
	for _, tc := range []struct {
		uri       string
		domain    string
		wantValid bool
	}{
		{"https://git.acme.com/user/oauth2/orglane/callback", "", true},
		{"http://localhost:8080/cb", "", true},
		{"http://127.0.0.1/cb", "", true},
		{"http://[::1]:9000/cb", "", true},
		// Not configured (domain mode): no *.localhost over http.
		{"http://git.orglane.localhost/user/oauth2/orglane/callback", "", false},
		{"http://orglane.localhost/cb", "", false},
		// Configured: the domain and its subdomains, with or without a port.
		{"http://git.orglane.localhost/user/oauth2/orglane/callback", local, true},
		{"http://git.orglane.localhost:8080/cb", local, true},
		{"http://orglane.localhost/cb", local, true},
		{"http://a.b.orglane.localhost/cb", local, true},
		// Everything that only looks like it.
		{"http://git.orglane.localhost.evil.com/cb", local, false},
		{"http://gitorglane.localhost/cb", local, false},
		{"http://git.other.localhost/cb", local, false},
		{"http://other.localhost/cb", local, false},
		{"http://localhost.orglane.localhost.com/cb", local, false},
		{"http://git.orglane.localhost./cb", local, false},
		{"http://GIT.orglane.localhost/cb", local, false},
		{"http://user@git.orglane.localhost/cb", local, false},
		{"http://user:pw@git.orglane.localhost/cb", local, false},
		{"http://git_x.orglane.localhost/cb", local, false},
		{"http://-git.orglane.localhost/cb", local, false},
		{"http://evil.com/cb?x=.orglane.localhost", local, false},
		{"http://evil.com/.orglane.localhost", local, false},
		{"http://10.0.0.1/cb", local, false},
		{"http://git.orglane.localhost/cb#frag", local, false},
		{"ftp://git.orglane.localhost/cb", local, false},
	} {
		err := validateRedirectURI(tc.uri, false, tc.domain)
		if (err == nil) != tc.wantValid {
			t.Errorf("%s (domain %q): err = %v, want valid %v", tc.uri, tc.domain, err, tc.wantValid)
		}
	}
}

// Only localhost and *.localhost names become local domains.
func TestWithLocalServiceDomain(t *testing.T) {
	for base, want := range map[string]string{
		"orglane.localhost":     "orglane.localhost",
		"localhost":             "localhost",
		"acme.com":              "",
		"orglane.localhost.com": "",
		"Orglane.localhost":     "",
		"":                      "",
	} {
		s := &Service{}
		WithLocalServiceDomain(base)(s)
		if s.localDomain != want {
			t.Errorf("%q: local domain %q, want %q", base, s.localDomain, want)
		}
	}
}
