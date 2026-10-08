// Package openapi holds the contract of the management API, so that the
// binary can serve it. admin.yaml is the source; admin.json is derived from
// it by `make gen`.
package openapi

import _ "embed"

//go:embed admin.yaml
var adminYAML []byte

//go:embed admin.json
var adminJSON []byte

// AdminYAML returns the contract as it was written.
func AdminYAML() []byte { return adminYAML }

// AdminJSON returns the contract as JSON.
func AdminJSON() []byte { return adminJSON }
