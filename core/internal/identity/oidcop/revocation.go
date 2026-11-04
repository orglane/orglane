package oidcop

import (
	"context"
	"net/http"
	"net/url"

	httphelper "github.com/zitadel/oidc/v3/pkg/http"
	"github.com/zitadel/oidc/v3/pkg/oidc"
	"github.com/zitadel/oidc/v3/pkg/op"
)

// The token parameter of a revocation request.
//
// zitadel's revocation endpoint (op.Revoke) does not check that a request
// has a token, which RFC 7009 (section 2.1) requires, and answers 200 as for
// an unknown token. Its newer op.Server does check, after authenticating the
// client. revocationCheck does the same for op.Revoke: it marks a request
// whose token is missing, empty or given more than once, and RevokeToken,
// which zitadel calls only once the client is authenticated, answers
// invalid_request (RFC 6749, section 5.2). Client authentication errors keep
// their answers.
//
// A repeated token is rejected (RFC 6749, section 3.1: parameters must not
// be included more than once) rather than resolved, because the token
// zitadel revokes is the one its form decoder picks: the last value of a
// key, of whichever key it decodes last when several keys match (keys match
// field names case-insensitively, "Token" and "token" alike, and a promoted
// field also by its dotted path, "RevocationRequest.token"; map iteration
// order decides between them). So the check asks the provider's own decoder
// which keys of the request it would decode into the token, and counts their
// values: with exactly one, the check and zitadel see the same token.
//
// The token_type_hint parameter can be ambiguous in the same way, but
// nothing here depends on it: the hint only decides in which order zitadel
// looks the token up, and every order revokes the same token.
type revocationCheck struct {
	// decoder is the provider's form decoder, set once the provider is
	// built (before it serves requests).
	decoder httphelper.Decoder
}

// revocationForm has the shape of the form op.ParseTokenRevocationRequest
// decodes, so a key maps to its Token field exactly when it maps to the
// token there.
type revocationForm struct {
	oidc.RevocationRequest
	oidc.ClientAssertionParams
	ClientID     string `schema:"client_id"`
	ClientSecret string `schema:"client_secret"`
}

type invalidTokenParamKey struct{}

// intercept is an interceptor of the provider's router, so it runs inside
// its CORS handling. It parses the form of a revocation request, so a form
// that cannot be read is answered here, as op.Revoke would.
func (c *revocationCheck) intercept(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == PathRevoke {
			if err := r.ParseForm(); err != nil {
				op.RevocationRequestError(w, r, oidc.ErrInvalidRequest().WithDescription("unable to parse request").WithParent(err))
				return
			}
			if invalid := c.invalidToken(r.Form); invalid != "" {
				r = r.WithContext(context.WithValue(r.Context(), invalidTokenParamKey{}, invalid))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// invalidToken returns why the token of a revocation form is invalid, or ""
// if it has exactly one non-empty token.
func (c *revocationCheck) invalidToken(form url.Values) string {
	var tokens []string
	for key, values := range form {
		var probe revocationForm
		if err := c.decoder.Decode(&probe, url.Values{key: {"probe"}}); err != nil {
			// op.Revoke fails to decode the form too and answers that.
			return ""
		}
		if probe.Token != "" {
			tokens = append(tokens, values...)
		}
	}
	switch {
	case len(tokens) > 1:
		return "token repeated"
	case len(tokens) == 0 || tokens[0] == "":
		return "token missing"
	}
	return ""
}

// invalidTokenParam returns why the token parameter of the revocation
// request in ctx is invalid, or "".
func invalidTokenParam(ctx context.Context) string {
	invalid, _ := ctx.Value(invalidTokenParamKey{}).(string)
	return invalid
}
