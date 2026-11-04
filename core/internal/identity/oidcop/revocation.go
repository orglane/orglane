package oidcop

import (
	"net/http"

	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// The token parameter of a revocation request.
//
// zitadel's revocation endpoint (op.Revoke) does not check that a request
// has a token, which RFC 7009 (section 2.1) requires, and answers 200 as for
// an unknown token. Its newer op.Server does check, after authenticating the
// client. revocationCheck does the same for op.Revoke: it runs zitadel's own
// op.ParseTokenRevocationRequest, which op.Revoke runs first (the same form,
// decoder and request type, and client authentication), and answers
// invalid_request (RFC 6749, section 5.2) when the token it decodes is
// empty. Errors of that step are answered as op.Revoke answers them, so
// client authentication and malformed forms keep their answers.
//
// op.Revoke then parses the request again, with the same result: client
// authentication only reads from the database. Which of several token
// values the decoder picks can differ between the two decodes (when
// differently cased keys carry tokens, map iteration order decides), but
// not whether the token is empty: the decoder takes the last value of each
// key and skips a key whose last value is empty, so the token is empty
// exactly when every key it reads as the token ends with an empty value. A
// token given more than once is not rejected; zitadel revokes the value its
// decoder picks.
type revocationCheck struct {
	// revoker is the provider, set once it is built (before it serves
	// requests).
	revoker op.Revoker
}

// intercept is an interceptor of the provider's router, so it runs inside
// its CORS handling.
func (c *revocationCheck) intercept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathRevoke {
			// op.ParseTokenRevocationRequest and op.Revoke each parse a
			// copy of r (WithContext): parsing r first gives both the same
			// form, and a failed ParseForm does not fail again, so its error
			// is answered here as op.ParseTokenRevocationRequest answers it.
			if err := r.ParseForm(); err != nil {
				op.RevocationRequestError(w, r, oidc.ErrInvalidRequest().WithDescription("unable to parse request").WithParent(err))
				return
			}
			token, _, _, err := op.ParseTokenRevocationRequest(r, c.revoker)
			if err != nil {
				op.RevocationRequestError(w, r, err) // as op.Revoke answers it
				return
			}
			if token == "" {
				op.RevocationRequestError(w, r, oidc.ErrInvalidRequest().WithDescription("token missing"))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
