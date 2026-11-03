package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/orglane/orglane/core/internal/api/apigen"
)

// validatePersonUpdate bounds and strictly decodes the public base-field
// contract after authentication and before the generated request decoder.
// Unknown fields must not silently appear to edit status, roles or fields
// governed by the central profile service.
func validatePersonUpdate(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Profile update is too large or unreadable.")
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var body apigen.PersonUpdate
	if err := dec.Decode(&body); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid profile update fields.")
		return false
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		writeProblem(w, r, http.StatusBadRequest, "Expected a single profile update.")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}
