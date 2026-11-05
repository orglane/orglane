package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orglane/orglane/core/internal/apps"
	githubprovider "github.com/orglane/orglane/providers/github"
	"github.com/orglane/orglane/providers/provider"
)

// Link failures map to safe, stable codes; nothing upstream leaks.
func TestLinkErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{&provider.Error{Code: "invalid_code", Err: errors.New("upstream detail")}, http.StatusBadRequest, "invalid_code"},
		{&provider.Error{Code: githubprovider.CodeRateLimited, Retryable: true, RetryAfter: 90 * time.Second}, http.StatusTooManyRequests, githubprovider.CodeRateLimited},
		{&githubprovider.APIError{Status: 502}, http.StatusBadRequest, "provider_unavailable"},
		{apps.ErrAccountAlreadyLinked, http.StatusConflict, "account_already_linked"},
		{apps.ErrInvalidState, http.StatusConflict, "invalid_state"},
		{fmt.Errorf("%w: no instance", apps.ErrNotSupported), http.StatusConflict, "not_supported"},
	} {
		var got *apiError
		if !errors.As(linkError(tc.err), &got) || got.status != tc.status || got.code != tc.code {
			t.Errorf("%v mapped to %+v, want %d %s", tc.err, got, tc.status, tc.code)
		}
		if got != nil && strings.Contains(got.detail, "upstream detail") {
			t.Errorf("%v leaked its cause", tc.err)
		}
	}
	var limited *apiError
	if errors.As(linkError(&provider.Error{Code: githubprovider.CodeRateLimited, Retryable: true, RetryAfter: 90 * time.Second}), &limited) && limited.retryAfter != "90" {
		t.Errorf("Retry-After = %q", limited.retryAfter)
	}
	// Internal failures stay internal errors (500), not client errors.
	internal := errors.New("database down")
	if got := linkError(internal); !errors.Is(got, internal) {
		t.Errorf("internal error became %v", got)
	}
}
