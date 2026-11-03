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
// enforces closed objects and required non-null fields. Diagnostics never echo input.
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
	if !importMappingHasRequiredFields(data) {
		writeProblem(w, r, http.StatusBadRequest, "Import mapping requires non-null fields.")
		return false
	}
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true
}

// importMappingHasRequiredFields retains presence information lost by generated
// value fields. Explicit zero, false and empty arrays remain valid; omitted or
// null required fields and null array items do not satisfy the contract.
func importMappingHasRequiredFields(data []byte) bool {
	var required struct {
		Columns []*struct {
			Column *int    `json:"column"`
			Target *string `json:"target"`
		} `json:"columns"`
		CreateTeams     *bool  `json:"createTeams"`
		SendInvitations *bool  `json:"sendInvitations"`
		SkipRows        []*int `json:"skipRows"`
	}
	if err := json.Unmarshal(data, &required); err != nil || required.Columns == nil || required.CreateTeams == nil || required.SendInvitations == nil || required.SkipRows == nil {
		return false
	}
	for _, column := range required.Columns {
		if column == nil || column.Column == nil || column.Target == nil {
			return false
		}
	}
	for _, row := range required.SkipRows {
		if row == nil {
			return false
		}
	}
	return true
}
