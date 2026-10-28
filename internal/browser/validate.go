package browser

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/vault"
)

// collections lists the record collections in canonical order (API.md §16). A collection's
// position fixes its bit in a selection and its place in GET /browser/usage.
var collections = [...]string{"bookmarks", "history", "tabs", "prefs"}

// selection is a set of collections as a bit mask (bit i = collections[i]).
type selection uint8

const allCollections = selection(1<<len(collections) - 1)

func collectionBit(name string) selection {
	for i, c := range collections {
		if c == name {
			return 1 << i
		}
	}
	return 0
}

func validCollection(name string) bool { return collectionBit(name) != 0 }

// names returns the selected collections in canonical order.
func (s selection) names() []string {
	out := make([]string, 0, len(collections))
	for i, c := range collections {
		if s&(1<<i) != 0 {
			out = append(out, c)
		}
	}
	return out
}

// sqlFilter returns the SQL condition " AND collection IN (?, …)" with its arguments, or
// nothing when every collection is selected. Collection names come from the fixed list above.
func (s selection) sqlFilter() (string, []any) {
	if s == allCollections {
		return "", nil
	}
	names := s.names()
	args := make([]any, len(names))
	for i, n := range names {
		args[i] = n
	}
	return ` AND collection IN (?` + strings.Repeat(", ?", len(names)-1) + `)`, args
}

var collectionsHint = strings.Join(collections[:], ", ")

// parseSelection parses the collections query parameter: a comma-separated list of collection
// names; empty or absent selects all. An unknown (or empty) name is a 422.
func parseSelection(v string) (selection, error) {
	if strings.TrimSpace(v) == "" {
		return allCollections, nil
	}
	var s selection
	for _, part := range strings.Split(v, ",") {
		name := strings.TrimSpace(part)
		b := collectionBit(name)
		if b == 0 {
			return 0, core.Validation(map[string]string{"collections": fmt.Sprintf(
				"unknown collection %q (allowed: %s)", clip(name, 32), collectionsHint)})
		}
		s |= b
	}
	return s, nil
}

// idPattern is the record id format (API.md §16): chosen by the client, bound into the AAD.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func validRecordID(s string) bool { return len(s) <= 64 && idPattern.MatchString(s) }

// checkKeyID validates the key_id a write must name (the current vault key, VAULT-CRYPTO.md §2.2).
func checkKeyID(id string) error {
	switch {
	case id == "":
		return core.Validation(map[string]string{"key_id": "is required (the key_id of the current vault key)"})
	case !vault.ValidKeyID(id):
		return core.Validation(map[string]string{"key_id": "must be a lowercase UUID"})
	}
	return nil
}

// Per-change error codes of POST /browser/records.
const (
	codeValidation = "validation_failed"
	codeQuota      = "quota_exceeded"
)

// change is one parsed entry of POST /browser/records.
type change struct {
	Collection, ID string
	Data           *string // nil: delete
	Base           *int64  // nil: unconditional (last writer wins)
	// errCode and errMsg say why the change cannot be applied ("" = valid).
	errCode, errMsg string
}

type changeInput struct {
	Collection   string          `json:"collection"`
	ID           string          `json:"id"`
	Data         json.RawMessage `json:"data"`          // absent: nil; null: "null"
	BaseRevision json.RawMessage `json:"base_revision"` // absent or null: unconditional
}

// parseChange decodes and checks one change. Every problem is reported on the change itself
// (validation_failed, or quota_exceeded for a record over the size limit) so that the other
// changes of the batch still apply. data must be present: a sealed string, or null to delete
// (a forgotten field must never delete a record).
func parseChange(raw json.RawMessage) change {
	var in changeInput
	err := json.Unmarshal(raw, &in)
	ch := change{Collection: in.Collection, ID: in.ID}
	fail := func(code, msg string) change {
		ch.errCode, ch.errMsg = code, msg
		return ch
	}
	if t := bytes.TrimSpace(raw); err != nil || len(t) == 0 || t[0] != '{' {
		return fail(codeValidation, "a change must be an object {collection, id, data, base_revision}")
	}
	if !validCollection(in.Collection) {
		return fail(codeValidation, "collection must be one of "+collectionsHint)
	}
	if !validRecordID(in.ID) {
		return fail(codeValidation, "id must match ^[A-Za-z0-9_-]{1,64}$")
	}
	if len(in.BaseRevision) > 0 && string(in.BaseRevision) != "null" {
		var n int64
		if err := json.Unmarshal(in.BaseRevision, &n); err != nil || n < 0 {
			return fail(codeValidation, "base_revision must be null or an integer of at least 0")
		}
		ch.Base = &n
	}
	switch {
	case len(in.Data) == 0:
		return fail(codeValidation, "data is required: a sealed string, or null to delete the record")
	case string(in.Data) == "null":
		return ch
	}
	var s string
	if err := json.Unmarshal(in.Data, &s); err != nil {
		return fail(codeValidation, "data must be a sealed string or null")
	}
	if len(s) > MaxRecordBytes {
		return fail(codeQuota, fmt.Sprintf("data must be at most %d characters (64 KiB per record)", MaxRecordBytes))
	}
	if msg := vault.CheckSealed(s, MaxRecordBytes); msg != "" {
		return fail(codeValidation, "data "+msg)
	}
	ch.Data = &s
	return ch
}

// clip shortens untrusted strings echoed back in results and messages.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
