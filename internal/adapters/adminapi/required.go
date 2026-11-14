package adminapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/albievan/cactusfs/internal/domain"
)

// requiredFields are, for each operation as the router matches it whose
// JSON body the contract gives required fields, those fields - at the top
// of the body, as the contract lists them in `required` (a test holds the
// two together). The generated decoder does not enforce them: a field that
// is not there is decoded as its zero value, and a body of
// {"password": "..."} to PUT /users/{name} would take the user's groups and
// roles away and enable a disabled user. A body without one of them is
// refused before the operation is carried out.
func requiredFields() map[string][]string {
	return map[string][]string{
		"POST " + BasePath + "/session":                       {"username", "password"},
		"POST " + BasePath + "/session/unlock":                {"password"},
		"POST " + BasePath + "/buckets":                       {"name"},
		"PUT " + BasePath + "/buckets/{bucket}/quota":         {"quota_bytes"},
		"PUT " + BasePath + "/buckets/{bucket}/policy":        {"document"},
		"PUT " + BasePath + "/buckets/{bucket}/tags":          {"tags"},
		"PUT " + BasePath + "/buckets/{bucket}/tag-rules":     {"lowercase", "catalogue_only", "required"},
		"PUT " + BasePath + "/buckets/{bucket}/object/tags":   {"tags"},
		"PUT " + BasePath + "/buckets/{bucket}/notifications": {"rules"},
		"PUT " + BasePath + "/buckets/{bucket}/replication":   {"target", "destination_bucket", "rules"},
		"PUT " + BasePath + "/buckets/{bucket}/replica":       {"source"},
		"POST " + BasePath + "/users":                         {"name"},
		"PUT " + BasePath + "/users/{name}":                   {"groups", "roles", "disabled"},
		"POST " + BasePath + "/groups":                        {"name", "roles"},
		"PUT " + BasePath + "/groups/{name}":                  {"roles"},
		"POST " + BasePath + "/roles":                         {"name", "policies"},
		"PUT " + BasePath + "/roles/{name}":                   {"policies"},
		"POST " + BasePath + "/policies":                      {"name", "document"},
		"PUT " + BasePath + "/policies/{name}":                {"document"},
		"POST " + BasePath + "/upgrade":                       {"version", "password"},
		"PUT " + BasePath + "/targets/{name}":                 {"kind", "buckets"},
		"POST " + BasePath + "/tag-definitions":               {"key"},
		"PUT " + BasePath + "/tag-definitions/{key}":          {"description", "values"},
	}
}

// policyFields are, for each operation as the router matches it whose JSON
// body carries a policy document, the field that holds it. The document
// reaches the parser as it was sent (api.PolicyDocument is a
// json.RawMessage), and the parser refuses a key given twice inside it; the
// generated decoder, though, matches the body's own keys without regard to
// case and keeps the last of two, so a body that gives the field twice -
// "document" and "Document" - would be judged by one document while a
// reader of the request saw the other. Such a body is refused.
func policyFields() map[string]string {
	return map[string]string{
		"PUT " + BasePath + "/buckets/{bucket}/policy":            "document",
		"PUT " + BasePath + "/buckets/{bucket}/subbuckets/{name}": "policy",
		"POST " + BasePath + "/policies":                          "document",
		"PUT " + BasePath + "/policies/{name}":                    "document",
		"POST " + BasePath + "/users/{name}/keys":                 "inline_policy",
	}
}

// refuseRepeatedPolicy refuses a body of an operation of policyFields that
// gives its policy field more than once, in any case. A body that is not a
// JSON object is left to the decoder, which refuses it. Nothing of the body
// is in the error.
func refuseRepeatedPolicy(pattern string, body []byte) error {
	field, ok := policyFields()[pattern]
	if !ok {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil
	}
	seen := false
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil
		}
		if key, _ := token.(string); strings.EqualFold(key, field) {
			if seen {
				return fmt.Errorf("%w: the body gives %q more than once", domain.ErrInvalidArgument, field)
			}
			seen = true
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil
		}
	}
	return nil
}

// missingField returns the first of fields that body does not give a value:
// a field that is not there, or is null. A key that is the field written in
// another case and is null counts as well: the generated decoder matches
// names without regard to case, and would decode that null over the value.
// The error wraps domain.ErrInvalidArgument for a body that is not a JSON
// object. Nothing of the body is in the error.
func missingField(body []byte, fields []string) (string, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(body, &object); err != nil || object == nil {
		return "", fmt.Errorf("%w: the body must be a JSON object", domain.ErrInvalidArgument)
	}
	null := func(value json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(value), []byte("null")) }
	for _, field := range fields {
		value, ok := object[field]
		if !ok || null(value) {
			return field, nil
		}
		for key, value := range object {
			if key != field && strings.EqualFold(key, field) && null(value) {
				return field, nil
			}
		}
	}
	return "", nil
}

// requireFields refuses a body that lacks a field the contract requires of
// the operation. It returns nil for an operation without such fields.
func requireFields(pattern string, body []byte) error {
	fields := requiredFields()[pattern]
	if len(fields) == 0 {
		return nil
	}
	missing, err := missingField(body, fields)
	if err != nil {
		return err
	}
	if missing != "" {
		return fmt.Errorf("%w: the body must give %q a value", domain.ErrInvalidArgument, missing)
	}
	return nil
}
