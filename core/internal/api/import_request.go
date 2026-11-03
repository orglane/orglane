package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/orglane/orglane/core/internal/api/apigen"
)

// validateImportMapping bounds the JSON mapping before generated decoding and
// enforces the contract's closed object shape. Diagnostics never echo input.
func validateImportMapping(w http.ResponseWriter, r *http.Request) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Import mapping is too large or unreadable.")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var mapping *apigen.PeopleImportMapping
	if err = decoder.Decode(&mapping); err != nil || mapping == nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid import mapping fields.")
		return false
	}
	if err = decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		writeProblem(w, r, http.StatusBadRequest, "Expected a single import mapping.")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}
