# ADR-0007: Management API, its documentation and the console

- **Status:** accepted
- **Date:** 2026-10-08
- **Milestone:** 5

## Context

Until now a deployment could only be used with S3 clients and its root credentials.
CLAUDE.md asks for a management API that is designed in OpenAPI before it is
implemented, served under `/api/v1` with generated handlers, `application/problem+json`
errors and `Idempotency-Key` on every mutation; Swagger UI from embedded assets; and a
React console that talks only to that API and holds no business or authorization logic.

## Decisions

### The contract comes first, and everything else is derived from it

- `api/openapi/admin.yaml`, **OpenAPI 3.0.3**: the highest version `oapi-codegen` (2.8.0)
  fully supports. `make gen` derives the Go server and types
  (`internal/adapters/adminapi/api`), the JSON form the binary serves
  (`api/openapi/admin.json`) and the console's TypeScript types
  (`web/src/api/schema.d.ts`). All three are committed; `make contracts` fails when
  regenerating changes anything, and `oasdiff` fails it on a change that breaks a client
  of the previous contract. A breaking change belongs in `/api/v2`.
- The generated *strict* server is used: an operation receives typed parameters and
  returns a typed answer, so a handler cannot answer with something the contract does
  not describe.
- Which operations need a credential is read from the contract: the generated routing
  marks them in the request context, and the adapter's authentication acts on that mark.
  There is no second list of public paths in the code.

### Authentication: an API key as a bearer credential, or a console session

- `Authorization: Bearer <access key ID>:<secret key>`. The secret is compared in
  constant time with the one unsealed through the KMS, exactly as for SigV4; a key
  counts as used only after that comparison succeeds.
- Or the session cookie that signing in sets (`__Host-`, `HttpOnly`, `Secure`,
  `SameSite=Strict`), with the CSRF token in `X-CSRF-Token` on every request that
  changes something. A JSON body must be sent as `application/json`, which a form on
  another site cannot do.
- Failed sign-ins are limited to ten in five minutes per client address, per node, in
  memory. An attempt is counted before the password is checked, so guesses made at the
  same moment count like guesses made one by one. A cluster allows as many attempts as
  it has gateways. The address is the peer's: behind a proxy or NAT every user shares
  one, and ten wrong guesses by anyone make all of them wait. Forwarded-for headers are
  not trusted. At most 32 password checks wait for one of the four hashing slots; beyond
  that sign-in answers 503.
- **Public, by necessity:** signing in, the contract (`/api/v1/openapi.*`), the
  documentation (`/docs`) and the files of the console. They reveal nothing about a
  deployment. This is an exception to "every inbound request is authenticated".

### Authorization stays where it was

The adapter makes no decision. Every operation calls a use case, and every use case asks
the single authorizer. The one new use case, cluster status, needs
`admin:GetClusterStatus`.

### Errors

One function maps the typed domain errors to `application/problem+json` (RFC 9457) with
a stable `code` and the request ID. A refusal (403) and an internal error (5xx) carry no
detail: what was protected, and what went wrong inside, is in the log and the audit
trail. A failed sign-in is always the same 401.

### Idempotency-Key

- A request with a key first **reserves** it in the replicated store, is carried out,
  and then leaves its **answer** there. A repeat with the same key and the same request
  gets that answer and changes nothing; the same key with another request is refused
  (422); a repeat while the first is in progress is refused (409). Answers are kept for
  24 hours.
- Keys are scoped to the caller: the API key, the root key or the user of a session.
- Answers are stored **sealed through the KMS** under a data key of their own, bound to
  the record: the answer to creating an API key holds its secret.
- A request is recognised by method, path, query and the SHA-256 of its body. An upload
  is recognised by bucket, key, length and media type, not by its content: reading it
  twice to find out is not worth it.
- **Not atomic with the change.** Reservation and answer are two Raft entries around the
  change, not one with it. A node that stops in between leaves a reservation without an
  answer; repeats are refused for fifteen minutes, and after that the request is carried
  out again. A reservation is removed only by the request that made it, or as the stale
  one that was read (the delete names the reservation's time), so a slow request cannot
  undo the repeat that took its key over. An upload that takes longer than fifteen
  minutes can be repeated beside itself. Staleness compares the clocks of two gateways. For everything named (users, buckets, policies) that second run answers with a
  conflict; for an API key it makes a second key, and the first is an orphan that is
  visible in the list and can be revoked. Making it atomic would put an idempotency
  record into every mutating Raft entry; that can still be done, compatibly.
- An answer of the server failing (5xx) or larger than 64 KiB is not kept.
- Signing in accepts the header and ignores it: an answer that holds a session is never
  given twice.
- A replayed answer is not authorized again. It is the answer the same caller was
  already given.
- The records are four new commands in Raft entry schema version 1
  (`idempotency.begin`, `.complete`, `.delete`, `.expire`). No release has shipped
  version 1, as with the IAM commands (ADR-0006).

### Objects through the management API

The console browses, uploads and downloads objects through `/api/v1`, with the same use
cases and the same authorizer as S3. Two things differ:

- A download is always `application/octet-stream` and `Content-Disposition: attachment`.
  Whatever was uploaded, a browser saves it and never renders it on the console's
  origin.
- An object stored this way gets an entity tag that is a SHA-256 of its content, cut to
  32 hex digits and marked `-1`, like the tag of a multipart upload. MD5 stays confined
  to the S3 adapter, where the protocol prescribes it.

### Swagger UI

`swagger-ui-dist` 5.33.1 (Apache-2.0), three files, committed under
`internal/adapters/adminapi/swaggerui/dist` with the integrity hash of the tarball they
came from, embedded with `go:embed` and served at `/docs`. The page loads nothing from
anywhere else, and its Content-Security-Policy (`default-src 'none'; script-src 'self';
style-src 'self'; ...`) makes a browser refuse to. "Authorize" takes an API key and
keeps it in the page only.

### Console

- React, TypeScript and Vite in `web/`. Two packages were added: `openapi-fetch`
  (runtime) and `openapi-typescript` (generator). No router, state library or UI kit.
- It calls the API only through the generated client, decides nothing about
  permissions, and shows the problem the API answers with.
- `web/embed_console.go` embeds `web/dist` under the build tag `console`;
  `web/embed_stub.go` is used without it, and the binary then serves a page that says
  the console was not built. `go build ./cmd/cactusfs` therefore still needs nothing but
  Go. Targets that compile with the tag (`lint`, `vet`, `build`) build the console
  first.
- Served by nodes with the `console` role on the admin listener, under the same strict
  Content-Security-Policy: no inline script or style, nothing from another host.
- The CSRF token is kept in the tab's `sessionStorage`. A second tab has the cookie but
  not the token, and signs in again.

### Cluster status

`GET /cluster` is the deployment as the answering node sees it: the nodes and topology
from its own copy of the replicated state, and every drive asked, at that moment and in
parallel, whether it answers. `GET /cluster/maintenance` is what the background work of
the **answering node** has done since it started. Scrubbing runs on the metadata leader
only, so another node reports no scrub pass. A cluster-wide view needs the nodes to ask
each other, which is left for later.

### Known limits

- The generated routing binds path and query parameters before the adapter
  authenticates. A request without a credential and with a missing parameter is
  answered 400, not 401. It learns nothing the public contract does not say.
- Reserving an idempotency key happens before the use case authorizes, so any
  authenticated caller can make two Raft entries and one KMS call per request, kept for
  a day. There is no cap per caller yet.
- Once an object transfer is running there is no progress timeout.
- Cookies are not scoped to a port, and `SameSite` treats the S3 port and the admin port
  of one host as one site. A page served from the S3 endpoint could make a signed-in
  browser send its cookie to `/api/v1`. The CSRF token and the JSON media type stop any
  change; reads are not cross-origin readable. This must be looked at again when
  milestone 6 makes objects reachable by presigned URL or anonymous policy.
- Signing in does not end the caller's previous session.

## Consequences

- The admin listener now carries small messages and object transfers. Its 15-second
  bound on a whole request stays for everything else and is lifted for an upload or
  download once the caller is authenticated.
- `/metrics` counts the API by route (`cactusfs_admin_requests_total`), never by path:
  names of buckets and users are not labels.
- The audit log can only be read forwards from a sequence number. The console's "newest"
  walks the pages.
- Not there yet: starting a scrub by hand, group membership seen from the group, OIDC
  (behind `ports.IdentityProvider`), a vulnerability gate for the console's packages.
