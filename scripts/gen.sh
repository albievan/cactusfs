#!/bin/sh
# Regenerates every file that is derived from an API contract. Generated code is
# committed, so a fresh clone builds with Go alone. Run through `make gen`.
#
# A generator is added here in the same step that adds its contract:
#   api/proto/             -> internal/adapters/noderpc/nodev1
#   api/openapi/admin.yaml -> internal/adapters/adminapi/api, api/openapi/admin.json,
#                             web/src/api/schema.d.ts
#
# api/asyncapi/events.yaml has no generated code: the event payloads are the
# structs in internal/ports/events.go, and a test holds the two together.
set -eu

module=$(go list -m)

# Node RPC. buf drives the two Go plugins, which are pinned in go.mod. The
# output is then formatted like every other Go file, so that the format gate
# has one rule.
buf generate
generated=internal/adapters/noderpc/nodev1
# The files are named one by one: given a directory, gofumpt leaves generated
# files alone, and `make fmt` and the format gate do not.
files=$(find "$generated" -name '*.go' | sort)
if [ -z "$files" ]; then
	echo "gen: FAIL - buf generated no Go file in $generated" >&2
	exit 1
fi
# shellcheck disable=SC2086 # the list is split on purpose
go tool gofumpt -w $files
# shellcheck disable=SC2086
go tool goimports -w -local "$module" $files
echo "gen: node RPC -> $generated"

# Management API. The Go server and its types come from oapi-codegen, pinned
# in go.mod; the JSON form of the contract, which the binary serves, and the
# TypeScript client of the console come from packages of web/.
contract=api/openapi/admin.yaml
generated=internal/adapters/adminapi/api/api.gen.go
go tool oapi-codegen -config api/openapi/oapi-codegen.yaml "$contract"
if [ ! -s "$generated" ]; then
	echo "gen: FAIL - oapi-codegen wrote no $generated" >&2
	exit 1
fi
go tool gofumpt -w "$generated"
go tool goimports -w -local "$module" "$generated"
echo "gen: management API -> $generated"

node scripts/openapi-json.mjs "$contract" api/openapi/admin.json
echo "gen: management API -> api/openapi/admin.json"

pnpm -C web run --silent gen:api
if [ ! -s web/src/api/schema.d.ts ]; then
	echo "gen: FAIL - openapi-typescript wrote no web/src/api/schema.d.ts" >&2
	exit 1
fi
echo "gen: management API -> web/src/api/schema.d.ts"
