#!/bin/sh
# Regenerates every file that is derived from an API contract. Generated code is
# committed, so a fresh clone builds with Go alone. Run through `make gen`.
#
# A generator is added here in the same step that adds its contract:
#   api/proto/             -> internal/adapters/noderpc   (PLAN 3.2)
#   api/asyncapi/events.yaml                              (PLAN 4.6)
#   api/openapi/admin.yaml -> internal/adapters/adminapi, web/src/api (PLAN 5.1, 5.5)
set -eu

echo "gen: no API contracts yet - nothing to generate"
