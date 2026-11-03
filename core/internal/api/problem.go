package api

import (
	"encoding/json"
	"net/http"

	"github.com/orglane/orglane/core/internal/api/apigen"
)

// ProblemContentType is the media type of RFC 9457 problem details.
const ProblemContentType = "application/problem+json"

// newProblem builds RFC 9457 problem details. The type is "about:blank", so
// the title is the HTTP status text; detail is optional.
func newProblem(r *http.Request, status int, detail string) apigen.Problem {
	p := apigen.Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
	}
	if detail != "" {
		p.Detail = &detail
	}
	if r != nil && r.URL != nil {
		instance := r.URL.Path
		p.Instance = &instance
	}
	return p
}

// writeProblem writes problem details as the response.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string) {
	body, err := json.Marshal(newProblem(r, status, detail))
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", ProblemContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}
