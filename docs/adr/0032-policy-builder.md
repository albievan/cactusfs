# ADR-0032: The visual policy builder - a catalogue, validation and an explanation from the node

- **Status:** accepted (2026-11-18, PLAN 9.12): implemented as written below, with the
  code of PLAN 9.1 to 9.10 and the gate of each - the catalogue, the located faults and
  the analysis, the explanation in the domain (9.1 to 9.3, with 9.2a), the port
  `ports.AccessExplainer` and the use cases `PolicyToolsService` and `ExplainService`
  (9.4, 9.5), the contract and its handlers (9.6), and the console's reader and writer,
  builder, bucket and sub-bucket editors and "Who may do this?" (9.7 to 9.10). No step
  changed the Raft entry schema, the on-disk format or the crypto design. Where the
  building went beyond or differed from the text below, the section
  [As built](#as-built-plan-91-to-910) says so; the decisions are unchanged. The points
  under "Open, for the owner" are not answered: their defaults are what is built (the
  threat model, questions 60 to 70, each with what was built; question 71 was added by
  the review of PLAN 9.5). The integration test on four nodes (PLAN 9.11,
  `test/integration/policybuilder_test.go`) is done; the review in a real browser (PLAN
  9.13) is to come. It was proposed on 2026-11-17 (PLAN 9.0) and
  revised that day to the review of PLAN 9.0: an explanation about a bucket also needs
  the right to read the policies it depends on; `Explain` is a port of its own
  (`ports.AccessExplainer`); the validation law is about the document;
  `unmatched_patterns`; the sub-bucket validation body; every explanation recorded
  before it is answered
- **Date:** 2026-11-17; accepted 2026-11-18
- **Builds on:** ADR-0006 (IAM: the policy language, strict parsing, `iam.Decide`, the
  built-in roles as part of the binary), ADR-0007 (the management API: problem+json,
  `Idempotency-Key`, CSRF), ADR-0011 (bucket policies and their principals), ADR-0017
  (sub-buckets and the three sources of a decision), ADR-0027 (a locked session),
  ADR-0028 (ownership of a version and `admin:PreviewObject`)

## Context

The console edits four kinds of policy, each as a box of JSON: a named policy (the
Policies screen), the inline policy of a new API key (`ApiKeys.tsx`), a bucket's policy
and a sub-bucket's (`SubBuckets.tsx`). What is typed is sent as typed (`typedPolicy`),
and the server parses it strictly (ADR-0006): a key given twice, an unknown field, a
condition key or operator it does not know are errors, and the first is told in the
problem's `detail`.

Albie asked for a builder made by choosing - who, what, where, when - beside the JSON it
comes to (PLAN, milestone 9). CLAUDE.md bars business and authorization logic from the
React app (sections 2 and 3). A builder that knew by itself which actions exist, what
`s3:Get*` covers, whether a statement opens a bucket to anyone, or why a request was
refused would be a second authorizer, written in another language, that drifts from the
one that decides. So the builder needs, from the node that answers it:

- the actions and condition keys there are, with what each allows and where it may be
  used;
- every fault of a document, located, without storing it;
- what each statement covers and whether it opens anything to anyone;
- the authorizer's own account of a decision, for "Who may do this?".

None of these exists in the management API today. Each is a disclosure of something,
and each must be the answer of the code that decides, not a copy of it.

## Decision

### 1. The catalogue

`GET /policy-catalogue` returns, from `iam.Catalogue()`, `iam.ConditionKeys()` and
`iam.PolicyLimits()` (PLAN 9.1):

- **every grantable action** - each `s3:` and `admin:` action of `internal/domain/iam`,
  and nothing else: no `cluster:` action (no policy can grant one) and none of the
  `auth:` and `system:` names of the audit trail (`app.ActionSignIn`,
  `app.OutboxDroppedAction` and the like). For each: its group, a sentence of what it
  allows, the resource templates it is asked on, and five flags, each the domain's own
  predicate: `read`, `by_name_only` (`Action.byNameOnly`: no wildcard of an Allow
  grants it), `in_bucket_policy` (an `s3:` action a bucket policy may allow),
  `anonymous` (`anonymousReadable`: a bucket policy can open it to a caller without
  credentials), `in_sub_bucket_policy` (`subBucketAction`);
- **the condition keys** `aws:SourceIp`, `aws:SecureTransport`, `aws:CurrentTime` and
  `aws:username`, each with its operators (`operatorsFor`), the kind of its values, and
  what an unknown value means (an unknown source address is decided against the
  request: it does not hold in an Allow and holds in a Deny);
- **the resource kinds** - `arn:aws:s3:::<bucket>`, `arn:aws:s3:::<bucket>/<key>`,
  `arn:cactusfs:admin:::<kind>[/<name>]` with the admin kinds the use cases ask on
  (`user`, `group`, `role`, `policy`, `audit`, `tag-definitions`, `updates` and the
  others) - and `*`;
- **the limits**: 20 KiB a document as sent (`MaxPolicyBytes`), 64 statements
  (`MaxPolicyStatements`), 128 actions, resources, principals or condition values a
  statement (`maxPolicyValues`), 8 levels of nesting (`maxPolicyDepth`), and the one
  `Version`, `2012-10-17`;
- **the principal forms** of a bucket policy (`parsePrincipal`): `"*"`, `{"AWS": "*"}`,
  `{"AWS": "arn:aws:iam:::user/<name>"}`, and a list of these under `AWS`.

The catalogue is a property of the binary, answered by the node asked: in a cluster
being upgraded, two nodes may answer two catalogues, and each validates by its own.
Every action enters it through a test that ties the constants of the package to it
(`TestCatalogueHoldsEveryAction`, go/parser over `internal/domain/iam`), every flag
through one that ties it to the predicate `Decide` uses (`TestCatalogueAgreesWithDecide`),
and every `x-cactusfs-action` of the contract must be in it
(`TestContractActionsAreInTheCatalogue`).

**A new condition key is a language change**, not an addition to the catalogue: an
older node re-parses every stored policy strictly and would refuse one that uses it.
It needs a cluster-level gate (as the level of the entry schema is one) and an ADR of
its own. Milestone 9 adds no condition key, no operator and no principal kind.

Two actions are new:

| Action | Asked on | Held by the built-in roles |
|---|---|---|
| `admin:GetPolicyCatalogue` | `arn:cactusfs:admin:::policy-catalogue` | `bucket-admin` (named in its policy), `auditor` (`admin:Get*`), `cluster-admin` (`admin:*`) |
| `admin:ExplainAccess` | `arn:cactusfs:admin:::user/<name>`, or `arn:cactusfs:admin:::anonymous` | `cluster-admin` only: it does not begin with Get or List |

`bucket-admin` gains `admin:GetPolicyCatalogue` because the builder of a bucket's policy
needs the catalogue and that role writes bucket policies; the catalogue tells nothing
that the contract and the source do not. The built-in policies are part of the binary
(ADR-0006), so this changes no stored state.

### 2. Validation

Three endpoints, one for each place a document is kept, each authorized by the action
that would store the document there, so that whoever may save may check and nobody
else may:

| Endpoint | Kind of document | Authorized as |
|---|---|---|
| `POST /policy-validation` | a named policy, or the inline policy of a new API key | `admin:PutPolicy` on `policy/<name>` (on `policy` when no name is given); with `key_owner`, `admin:CreateAPIKey` on `user/<key_owner>` |
| `POST /buckets/{bucket}/policy/validation` | the bucket's policy | `s3:PutBucketPolicy` on the bucket |
| `POST /buckets/{bucket}/subbuckets/{name}/policy/validation` | the sub-bucket's policy | `s3:PutSubBucket` on the bucket |

They **store nothing, and read nothing beyond what the put's own authorization reads**
(the authorizer reads the bucket's policy for an `s3:` action on a bucket), **and so say
nothing a put would not**: a bucket or sub-bucket that is not there is no error - its
name is used only to confine the document, as the parser does. They run the same parser
the put runs (`ParsePolicy`, `ParseBucketPolicy`, `ParseSubBucketPolicy`), on the bytes as
sent (`PolicyDocument` stays `json.RawMessage`; a body that gives its policy field twice
is refused by `refuseRepeatedPolicy`, as for every put).

A principal bound by an inline policy of its key (`Principal.Boundary` set) is refused a
validation with `key_owner`, and the refusal is audited, exactly as `CreateAPIKey`
refuses it ("an API key with a policy of its own cannot make keys", `app/iam.go`):
whoever may not make the key may not check its policy either.

The answer is 200 `PolicyValidation`: `valid`, the `faults`, and the analysis of each
statement. **A document is valid exactly when it has no fault**, and, **as far as the
document goes**, a put accepts exactly the documents its validation calls valid - a
fuzzed law of the domain (`FuzzPolicyFaults`, all three parsers) and a test on four
nodes (PLAN 9.11). The law is about the document only. A put of a valid document can
still be refused for what is not in it: a policy name that is built in
(`iam.IsBuiltIn`), a bucket that is not there (`NoSuchBucket`), the eleventh sub-bucket
of a bucket (`TooManySubBuckets`), a name a sub-bucket cannot have, an API key made by a
bound key, a key whose expiry has passed, a `create` of a policy that exists
(`EntityAlreadyExists`) - and, at another moment, a refusal by the authorizer.

A **fault** has an RFC 6901 pointer into the document as sent (`/Statement/2/Action/0`),
the index of its statement when it is in one, and a reason. A field that may be one
string or a list (`Action`, `Resource`, `Principal`'s `AWS`, a condition's values) is
pointed at with an index when it is written as a list and without one when it is
written as a single string: `/Statement/0/Action` is the action of
`"Action": "s3:GetObjet"`, `/Statement/0/Action/1` the second of a list. The parsers collect every
fault they can, at most 100 (`truncated` says there were more), and the reason of the
first is, word for word, the text the put's error has today. A fault of JSON syntax
carries a byte offset in the domain; it cannot arrive through this API, whose body is
JSON with the document in it, and the contract has no field for it.

One class is an exception to the law that the parsers accept and refuse what the frozen
parsers of 3e620ef did (PLAN 9.2a): a document followed, after white space, by a closing
bracket `}` or `]`, which those took and every parser now refuses as "data after the
document", its only fault, so that what is accepted is always JSON; a bucket's policy
already stored is read with the old tolerance (`iam.ParseStoredBucketPolicy`, called only
by the authorizer) and is shown as the document enforced (`iam.StoredPolicyDocument`),
while a sub-bucket's, never stored so, is read strictly.

The **analysis** says of each statement, from `iam.AnalysePolicy`,
`AnalyseBucketPolicy` and `AnalyseSubBucketPolicy` (PLAN 9.2):

- which actions of the catalogue its action patterns cover - a by-name-only action
  never by an Allow's wildcard, as `Statement.matches` has it;
- for a bucket's policy, whether it is open to anyone (an Allow whose principals
  include `*`, as `BucketPolicy.OpenToAnyone` has it);
- which of its actions a caller without credentials could really be let in to - the
  covered actions that are `anonymousReadable`, before conditions;
- which actions it names for anyone that are never anonymous (a statement that allows
  everyone `s3:PutObject`: every user, nobody unauthenticated);
- which of its action patterns cover no action of the answering node's catalogue
  (`s3:GetObjet`, `admin:Lst*`): the parser takes them - an action it does not know
  may be one of a newer release - so a misspelling is no fault, and this is where it
  shows.

The analysis classifies; it decides nothing. Conditions and other statements are not
weighed, and the answer says so, as `BucketPolicySummary` does today.

**Faults from every put.** The same faults are given with the 400 of every put that
refuses a document, in a new optional `faults` of `Problem`. The code of the 400 does
not change: `InvalidPolicy` for a bucket's policy (`putBucketPolicy`) and a
sub-bucket's (`putSubBucket`), whose errors wrap `domain.ErrInvalidPolicy`; and
`InvalidArgument` for a named policy (`createPolicy`, `updatePolicy`) and an API key's
inline policy (`createAPIKey`), whose errors wrap `domain.ErrInvalidArgument` only
(`app.refusedPolicy` says so of the identity use cases). A put authorizes before it
parses, so a caller without the put's action is told 403 and no fault.

The S3 API is unchanged: a refused bucket policy is still answered 400
`InvalidArgument` there (`s3api`, `sentinels_test.go`), with the first fault's text,
and the s3-compat baseline does not move.

### 3. "Who may do this?" - the explanation

`GET /access/explain` answers, for a subject, an action and a resource, what the
authorizer decides and why. The decision is the authorizer's own, through a port of its
own, narrow on purpose:

```go
// AccessExplainer reports the decision the Authorizer would make and why.
// It is not an Authorizer and never the gate of an operation: only the
// explanation use case holds one.
type AccessExplainer interface {
	// Explain reports the decision Authorize would make for req and why -
	// the same decide, from the same reading of the policies of the bucket
	// and the sub-bucket. Nothing is logged or audited.
	Explain(ctx context.Context, req iam.Request) (iam.Explanation, error)
}
```

`ports.AccessExplainer` is implemented by `app.Authorizer` (which keeps its one
`decide`) and given, in `cmd/cactusfs`, to `ExplainService` alone. `ports.Authorizer` does
not change, nor does any of its implementations and fakes (`porttest.Authorizer`,
`countingAuthorizer`, `switchAuthorizer`, `changedMind`, `allowAll`): no use case that
gates an operation can reach `Explain` through the port it holds. A test holds that,
by type: no constructor and no field of a use case but `ExplainService`'s takes a
`ports.AccessExplainer` (`TestOnlyTheExplainUseCaseHoldsAnExplainer`, go/types over
`internal/app` and the wiring of `cmd/cactusfs`).

Behind both ports, `iam.Explain(req) Explanation` is the one evaluation, of which `Evaluate` becomes the
projection (PLAN 9.3): allowed or not; the source (`iam.Source`); the rule when no policy
had a say (root, anonymous, service, node, locked session, no user); the statements that
matched, each with its source, the index of its document and of itself, its Sid and its
effect; the verdict of the key's inline policy; the conditions that were decided on an
address that was not known.

`ExplainService.Explain` (PLAN 9.5), in this order:

1. **Authorization of the subject**, `admin:ExplainAccess` on the subject, through
   `Authorize` (a refusal is audited as every refusal is): `user/<name>` for a user; for
   an access key the key is resolved first and asked about as `user/<owner>`, or as
   `user` when there is no such key - as `DeleteAPIKey` does - so that a refusal says
   nothing of the key; `anonymous` for a caller without credentials.
2. **Authorization of the policies the answer depends on.** An answer about a bucket
   tells, through its verdict, its `source`, the effects of its citations, and how it
   moves with `source_ip`, `secure_transport` and `list_prefix`, what the bucket's and a
   sub-bucket's policies say - and a stored policy that cannot be read would surface as
   an error of integrity. So, besides the subject:
   - a request bound by bucket policies - an `s3:` action with a `bucket` - needs
     `s3:GetBucketPolicy` on that bucket;
   - a request whose `key`, or whose `list_prefix`, could lie in a sub-bucket - its
     first segment can name one and a slash follows, as `subBucketOf` decides - needs
     `s3:GetSubBucketPolicy` on that bucket as well. Whether such a sub-bucket is
     registered is not asked first: a requirement that depended on it would tell it.

   Both are asked of the caller with `Allowed`; a "no" is a refusal of the whole
   request, 403 `AccessDenied`, audited as a refusal of `admin:ExplainAccess` with the
   reason ("may not read the policy the answer depends on"), with no part of an answer.
   A policy that cannot be read (`domain.ErrIntegrity`, which otherwise answers 500) is
   answered exactly as such a refusal - 403, audited - so that a damaged policy is told
   from a readable one only in the node's log. An answer about an `admin:` action, or
   `s3:ListAllMyBuckets`, depends on no bucket and needs only the first.
3. **The subject.** A user or key that is not there is answered 404 `NoSuchEntity`,
   after authorization. A disabled user, a key whose owner is disabled, and an expired
   key are answered 200 with `authenticates: false` and the reason, and no decision:
   such a request never reaches the authorizer. The root identity is not a subject: its
   access key is no key of the IAM store, and is answered as a key that is not there.
4. **The request**: the principal as authentication would make it - the user and the
   documents of their policies, with their names (`Principal.PolicyNames`, PLAN 9.4); for
   a key, also its inline policy as the boundary - never a locked session; the action,
   which must be one of the catalogue, without a wildcard; the resource, which must be of
   a template the catalogue gives the action; the listing prefix, for the three listing
   actions only; the source address and TLS the caller names; the time of this node -
   there is no simulation at another time.
5. **`AccessExplainer.Explain`**, which reads the policy of the bucket and of the sub-bucket
   as `Authorize` would.
6. **Citations filtered.** A cited statement is named - policy name, document and
   statement index, Sid - only where the caller may read where it is, asked with
   `Allowed` (no audit of each question): `admin:GetPolicy` on `policy/<name>` for a
   named policy, `s3:GetBucketPolicy` and `s3:GetSubBucketPolicy` on the bucket for
   those, `admin:ListAPIKeys` on `user/<owner>` for a key's inline policy. Otherwise the
   citation keeps its source and effect, and says "a policy you may not read". The
   verdict and its source are always given.
7. **The record.** Every request that passes authorization is recorded before it is
   answered, whatever the answer - a decision, `authenticates: false`, 404
   `NoSuchEntity`, 400 `InvalidArgument` - by one audit record (subject, action,
   resource, the answer: allowed and source, or the status and code; never a document or
   a statement) appended through `ports.AuditLog.AppendAudit`. Not the background queue
   of `Auditor.Succeeded`, which drops a record when it is full. When the record cannot
   be appended the answer is not given: 503 `ServiceUnavailable`, or 500 `InternalError`
   when the store cannot say whether it was appended (`domain.ErrOutcomeUnknown`, as
   for every change). A request refused by authorization is audited as a refusal, as
   every refusal is.

**Not considered**: the ownership of a version (ADR-0028). The creator of a version is
not read - telling it would need `admin:GetObjectAccess` - so `admin:PreviewObject` is
explained as for a user who did not commit the version, and the answer says that
ownership was not considered. Only that action asks who made a version, so only an
answer about `admin:PreviewObject` says so.

### 4. In memory only

`Principal.PolicyNames` and `Identity.PolicyNames` carry the names of the documents in
`Policies`, in their order; `ports.IAMStore.ResolveIdentity` and `ResolveUser` return
them (`raftmeta.resolveUser` has them as it reads; a policy it cannot find is left out
with its document, as now). No Raft command, entry, stored record or KMS use changes.
The one change of a built-in policy - `bucket-admin` gains `admin:GetPolicyCatalogue` -
is of the binary.

### 5. The console

- The JSON is what is sent, as shown. An unchanged document is never sent.
- The reader and the writer keep a law, `canonical(write(read(t))) = canonical(t)` for
  every text the reader takes (`canonical`: keys sorted, no whitespace, the form of each
  field kept - a string or a list, `"*"` or `{"AWS": "*"}`); it is tested against a corpus
  the Go parsers write (PLAN 9.7).
- What the reader cannot take - a key given twice (`JSON.parse` keeps the last), an
  unknown field, a wrong type - is kept as it is, and the builder is disabled with the
  reason. A statement, action or condition the answering node's catalogue does not list
  is kept and marked, never dropped.
- The writer indents, and goes compact above 20 KiB, as `policyLayout` does, so that a
  document is never refused for its spaces.
- The warning before a statement that opens a bucket to anyone is saved comes from the
  server's analysis, not from the console.
- A node of an older release answers the new paths 404 `NotFound` (the contract's rule
  for a path it does not name), and a caller without `admin:GetPolicyCatalogue` is
  answered 403 `AccessDenied` by the catalogue; on either the console offers the JSON
  editor alone, as today, with a short notice of why the builder is not there ("This
  node has no policy builder" or "You may not read the catalogue of actions").
- No `<style>` element is injected: the Content-Security-Policy is `style-src 'self'`.

## The contract

What PLAN 9.6 writes into `api/openapi/admin.yaml` (OpenAPI 3.0.3, as the document is).
Every operation answers `default` with `#/components/responses/Problem`; every one is
authenticated (`security` as the document's); 401 `Unauthenticated`, 403 `AccessDenied`
(no `detail`, audited) and 423 `SessionLocked` are as for every operation - none of the
new ones is open to a locked session. The paths are additions only; `oasdiff` must find
nothing breaking.

### Paths

| Path | Method | `operationId` | `x-cactusfs-action` | Tag |
|---|---|---|---|---|
| `/policy-catalogue` | GET | `getPolicyCatalogue` | `admin:GetPolicyCatalogue` | `policies` |
| `/policy-validation` | POST | `validatePolicy` | `admin:PutPolicy` (with `key_owner`: `admin:CreateAPIKey`, said in the description) | `policies` |
| `/buckets/{bucket}/policy/validation` | POST | `validateBucketPolicy` | `s3:PutBucketPolicy` | `buckets` |
| `/buckets/{bucket}/subbuckets/{name}/policy/validation` | POST | `validateSubBucketPolicy` | `s3:PutSubBucket` | `buckets` |
| `/access/explain` | GET | `explainAccess` | `admin:ExplainAccess` | `policies` |

**`getPolicyCatalogue`** - no parameters. 200 `application/json` `PolicyCatalogue`.

**`validatePolicy`** - parameters `#/components/parameters/IdempotencyKey` and
`#/components/parameters/CSRFToken`. Body (required) `application/json`
`PolicyValidationRequest`. 200 `PolicyValidation` - a document that is refused is a 200
with `valid: false`, never a 400. 400 `InvalidArgument` for a request that is not one: a
body that is not JSON, no `document`, `document` given twice, both `name` and
`key_owner`, a name that is not a `Name`. 403 `AccessDenied`, audited, also for a
validation with `key_owner` asked with an API key that has an inline policy, as
`createAPIKey` refuses it. The description says that `Idempotency-Key`
is accepted and has no effect, since nothing is changed, as for `signIn` and
`unlockSession`, and that the CSRF token is required with a session as for every POST.

**`validateBucketPolicy`** - parameters `BucketName`, `IdempotencyKey`, `CSRFToken`. Body
(required) `BucketPolicyPut` (`{document}`), the put's own shape. 200 `PolicyValidation`.
400 `InvalidArgument` as above. The description says that nothing is read beyond what
the authorization of the put reads: a bucket that is not there is no error.

**`validateSubBucketPolicy`** - parameters `BucketName`, `SubBucketName`,
`IdempotencyKey`, `CSRFToken`. Body (required) `SubBucketPolicyValidationRequest`
(`{policy}`): the put's own shape (`SubBucketPut`), with `policy` required - a sub-bucket
without a policy has nothing to validate. 200 `PolicyValidation`. 400 `InvalidArgument`
as above. Neither the bucket nor the sub-bucket is looked up.

**`explainAccess`** - query parameters:

| Name | Schema | Required | What |
|---|---|---|---|
| `user` | `Name` | one of the three | the user asked about, as a console session of theirs |
| `access_key_id` | string, 1 to 128 | one of the three | the API key asked about: its owner, bounded by its inline policy |
| `anonymous` | boolean, `true` only | one of the three | a caller without credentials |
| `action` | string, 1 to 128 | yes | an action of the catalogue, without a wildcard |
| `bucket` | string, 3 to 63 | for an `s3:` action but `s3:ListAllMyBuckets` | the bucket |
| `key` | string, 1 to 1024 | for an action on objects | the object key |
| `admin_resource` | string, 1 to 1024 | for an `admin:` action | `<kind>` or `<kind>/<name>`, as the catalogue's templates |
| `list_prefix` | string, 0 to 1024 | no; listing actions only | the prefix of the listing (decides whether a sub-bucket's policy has a say) |
| `source_ip` | string, an IPv4 or IPv6 address without a zone | no | the address the request comes from; absent, it is unknown and conditions on it are decided against the request |
| `secure_transport` | boolean | no, default `true` | whether the request arrives over TLS |

200 `AccessExplanation`. 400 `InvalidArgument`: none or more than one subject, an action
not in the catalogue or with a wildcard, a resource that is not of a template of the
action, a `list_prefix` for an action that lists nothing, a `source_ip` that is no
address. 404 `NoSuchEntity`: the user or key is not there (after authorization). 503
`ServiceUnavailable`: the audit record could not be appended, and no answer is given (500
`InternalError` when the store cannot say whether it was). 403 `AccessDenied`, audited,
also when the caller may not read the bucket's policy or a sub-bucket's that the answer
depends on, and when such a policy cannot be read (Decision 3, step 2). Every answer
after authorization - 200, 400 and 404 alike - is recorded before it is given. A
GET needs no CSRF token; the cookie is `SameSite=Strict`, and another site can neither
send it nor read the answer.

**The pipeline**: the patterns of the three validation POSTs join `unlockSession` and
`startUpgrade` among those whose `Idempotency-Key` is taken and not used
(`handler.go`); the three validation operations join the puts that take a policy in
`policyFields` (`required.go`) - `document` for `validatePolicy` and
`validateBucketPolicy`, `policy` for `validateSubBucketPolicy`, as for their puts - so a
repeated policy field is refused before the handler.

### Schemas

**`PolicyValidationRequest`** - `required: [document]`.

| Property | Schema | What |
|---|---|---|
| `document` | `PolicyDocument` | the document, read as sent |
| `name` | `Name` | the policy it would be stored as; decides the resource of the authorization |
| `key_owner` | `Name` | the user a new key with this inline policy would belong to; not with `name` |

**`SubBucketPolicyValidationRequest`** - `required: [policy]`; `policy` is a
`PolicyDocument`.

**`PolicyFault`** - `required: [pointer, reason]`.

| Property | Schema | What |
|---|---|---|
| `pointer` | string, RFC 6901 | where in the document as sent; `""` is the whole document. A field that may be a string or a list is pointed at without an index when it is a single string (`/Statement/0/Action`) and with one when it is a list (`/Statement/0/Action/1`) |
| `statement` | integer, minimum 0 | the index of the statement it is in, counted from 0 as the reasons count; absent outside a statement |
| `reason` | string | what is wrong, in the words of the parser |

**`PolicyValidation`** - `required: [valid, faults, truncated, statements]`.

| Property | Schema | What |
|---|---|---|
| `valid` | boolean | true exactly when `faults` is empty: the put would take the document - as a document; the put may still refuse for what is not in it (Decision 2) |
| `faults` | array of `PolicyFault`, `maxItems: 100` | every fault found, the first first |
| `truncated` | boolean | there were more than 100 |
| `statements` | array of `StatementAnalysis`, `maxItems: 64` | one a statement, in order; empty when the document is not valid |

**`StatementAnalysis`** - `required: [index, effect, actions, anonymous_actions, never_anonymous, unmatched_patterns]`.

| Property | Schema | What |
|---|---|---|
| `index` | integer | the statement's index |
| `sid` | string | its Sid, when it has one |
| `effect` | string, enum `Allow`, `Deny` | |
| `actions` | array of string | the actions of the answering node's catalogue its patterns cover |
| `open_to_anyone` | boolean | a bucket's or sub-bucket's policy only: an Allow whose principals include `*` |
| `anonymous_actions` | array of string | of `actions`, those a caller without credentials could be let in to, before conditions; empty but in an open statement of a bucket's policy |
| `never_anonymous` | array of string | of `actions`, those the statement names for anyone that are never allowed without credentials |
| `unmatched_patterns` | array of string | the statement's action patterns, as written, that cover no action of the answering node's catalogue - a misspelling, or an action of another release |

**`PolicyCatalogue`** - `required: [version, actions, condition_keys, resources, limits, bucket_policy_principals]`.

| Property | Schema | What |
|---|---|---|
| `version` | string | `2012-10-17`, the one `Version` |
| `actions` | array of `CatalogueAction` | every grantable action |
| `condition_keys` | array of `CatalogueConditionKey` | the four keys |
| `resources` | array of `CatalogueResource` | the resource kinds and their templates |
| `limits` | `PolicyLimits` | `max_bytes` 20480, `max_statements` 64, `max_values` 128, `max_depth` 8 |
| `bucket_policy_principals` | object: `anyone` (array of the forms, as JSON text), `user_arn_prefix` (`arn:aws:iam:::user/`) | the forms a bucket policy's `Principal` may take |

`CatalogueAction` - `required` all of: `name` (string), `group` (string), `description`
(string), `resources` (array of template strings, such as `arn:aws:s3:::{bucket}/{key}`
or `arn:cactusfs:admin:::user/{name}`), `read`, `by_name_only`, `in_bucket_policy`,
`anonymous`, `in_sub_bucket_policy` (booleans). `CatalogueConditionKey` - `key`,
`operators` (array of string), `value_kind` (enum `ip_prefix`, `bool`, `date_time`,
`string`), `when_unknown` (string, what an unknown value means). `CatalogueResource` -
`kind`, `template`, `description`. Every enum of these schemas says that values are only
ever added.

**`AccessExplanation`** - `required: [subject, request, authenticates, assumed, not_considered]`.

| Property | Schema | What |
|---|---|---|
| `subject` | object: `kind` (enum `user`, `access_key`, `anonymous`), `user`, `access_key_id` | whom it is about |
| `request` | object: `action`, `resource` (the ARN), `list_prefix` | what was decided |
| `authenticates` | boolean | false: the subject would not authenticate, and nothing below is given |
| `not_authenticated` | string, enum `user_disabled`, `key_expired` | why, when `authenticates` is false |
| `allowed` | boolean | the decision; absent when `authenticates` is false |
| `source` | string, enum `none`, `rule`, `identity-policy`, `bucket-policy`, `sub-bucket-policy`, `api-key-policy`, `owner` | `iam.Source` (`owner` is not given while ownership is not considered) |
| `rule` | string, enum `root`, `anonymous`, `service`, `node`, `locked_session`, `no_user` | when `source` is `rule` |
| `statements` | array of `CitedStatement` | the statements that matched, of every source |
| `key_policy` | object: `allowed` (boolean), `statements` (array of `CitedStatement`) | a key with an inline policy only: what its own policy said |
| `unknown_address` | array of `CitedStatement` | statements whose condition on `aws:SourceIp` was decided without an address |
| `assumed` | object: `time` (date-time, the node's), `source_ip` (absent: unknown), `secure_transport` (boolean) | what the decision was made with |
| `not_considered` | array of string, enum `object_ownership` | what the answer leaves out: `object_ownership` when, and only when, the action is `admin:PreviewObject` - the one action that asks who made a version; empty otherwise |

`CitedStatement` - `required: [source, effect, readable]`: `source` (as above),
`effect` (`Allow`, `Deny`), `readable` (boolean), and, only when `readable` is true,
`policy` (the name of a named policy; absent for a bucket's, a sub-bucket's or a key's),
`document_index`, `statement_index` (integers, from 0) and `sid`. When `readable` is
false the console says "a policy you may not read".

**`Problem`** gains one optional property, `faults`: an array of `PolicyFault`,
`maxItems: 100`, present only on a 400 that refuses a policy document - `InvalidPolicy`
from `putBucketPolicy` and `putSubBucket`, `InvalidArgument` from `createPolicy`,
`updatePolicy` and `createAPIKey`. The descriptions of those operations say so.

## Consequences

- **The hot path of the authorizer is refactored.** `Evaluate` becomes the projection
  of `Explain`, and `judge` reports matches without allocating on the path of
  `Decide`. `TestDecideAllocations` holds the allocations of `Decide` at the figure
  recorded before the change and `BenchmarkDecide` within 5 %; `FuzzExplainAgreesWithDecide`
  holds the two to one decision.
- **The located-fault parser could drift from accept and refuse.** `FuzzPolicyFaults`
  and every existing parser test, unchanged, hold it; the text of the first fault is
  the error of today, so the S3 API's 400 `InvalidArgument` and the s3-compat baseline
  do not move.
- **Every future action must enter the catalogue**, or `TestCatalogueHoldsEveryAction`
  fails; every `x-cactusfs-action`, or `TestContractActionsAreInTheCatalogue` does.
- **A mixed-version cluster.** Older nodes answer the new paths 404 `NotFound`; the
  console falls back to the JSON editor. A node of this release validates by its own
  catalogue. A new condition key later needs a cluster-level gate (Decision 1).
- **The audit trail grows** by one record for every explanation, written before the
  answer: an explanation costs a Raft entry, and fails when the trail cannot be written.
- **Disclosure.** An explanation tells what another identity may do, and through its
  verdict what the policies it depends on say. It is held to `cluster-admin` by default;
  an answer about a bucket also needs `s3:GetBucketPolicy` on it, and one that could be
  in a sub-bucket `s3:GetSubBucketPolicy`; a damaged policy is answered as a refusal;
  its citations are filtered by what the caller may read; and every answer is recorded.
- **Wildcard grants gain the new actions on upgrade.** A stored policy that allows
  `admin:*` - or `admin:E*`, `admin:*Access` - on `arn:cactusfs:admin:::user/*` or `*`
  holds `admin:ExplainAccess` from the release that brings it, without anybody granting
  it; one that allows `admin:Get*` holds `admin:GetPolicyCatalogue`. The second
  discloses nothing; the first discloses, about admin actions, what the users it covers
  may do (open question 70).
- **The regeneration** of the Go and TypeScript clients needs Docker on the test host;
  it is done once, in PLAN 9.6.
- **The 20 KiB bound counts the builder's indentation**: the writer goes compact above
  it, as `policyLayout` does.

**Threat model, section 4** (written at PLAN 9.12):

- new rows: "An explanation discloses the permissions of others" (`admin:ExplainAccess`
  for `cluster-admin` only, citations filtered, every answer recorded before it is
  given); "An explanation probes policies the caller may not read" (an answer about a
  bucket needs `s3:GetBucketPolicy`, one that could be in a sub-bucket
  `s3:GetSubBucketPolicy`, both asked before anything is decided; a policy that cannot
  be read is answered as a refusal; a wildcard grant that gains `admin:ExplainAccess` on
  upgrade is open question 70); "The catalogue invents or hides an action" (the
  catalogue is the domain's, tied to the constants and to `Decide` by tests);
  "Validation as an oracle" (it reads nothing beyond what the put's own authorization
  reads, is authorized as the put, and so says nothing a put would not); "The builder
  changes a document silently" (the round-trip law, the JSON sent as shown, what cannot
  be read kept and marked);
- changed rows: "A policy is applied differently from how it reads" (now also the
  builder and the validation routes); "A bucket is opened to the world by mistake or by
  a stolen key" (the warning, from the server's analysis); "A handler forgets an
  authorization check" (`Explain` is behind a port of its own that no gating use case
  holds, and a test holds that by type).

The threat model has these rows since PLAN 9.12: the five new ones under "The visual
policy builder (ADR-0032)", and the three changed rows where they were.

## As built (PLAN 9.1 to 9.10)

What was built is what the decisions above say. Where the building went further, or
did a thing otherwise than the text above describes, it is listed here, with the PLAN
row ("<step> as written") that has the detail. Nothing here changes a decision.

- **Stricter checks at save in the bucket editors** (9.9). Decision 5 gives the plain
  JSON editor where the builder is not there. The editors of a bucket's and a
  sub-bucket's policy (`web/src/screens/BucketPolicyEditor.tsx`) still check at save
  whenever the builder is not shown - the catalogue refused (403), failed or later than
  10 s - with one validation of the bytes the put will send, authorized by the put's own
  action, so a caller who may save causes no false audited refusal; nothing with faults
  is sent. On a node of an older release the bucket's policy is checked the same way.
  The identity-policy screens keep Decision 5's fallback. Validation is asked only for a
  document that differs from the stored one, never as an editor opens (9.8), since a
  reader without the put's action would otherwise be refused and audited for a change
  nobody asked for.
- **The "could not check" question** (9.9). When a bucket's policy cannot be checked -
  validation answers 404 `NotFound`, whatever the catalogue answered - the save does not
  take that for "not open": it asks "This node could not check the policy, so it cannot
  say whether it opens *bucket* to anyone. Save it as it is?". A sub-bucket's policy is
  not asked about on an older node: its put refuses every Allow to `*` (ADR-0017). The
  "anyone" warning itself is titled "to anyone", or "to every user of this deployment"
  when no open statement has `anonymous_actions`.
- **The catalogue is read lazily** (the 9.9 amendment of 9.8, and 9.10). The console
  reads `GET /policy-catalogue` only on the first edit of an editable policy or when
  "Open the policy builder" is pressed - on "Who may do this?", on the first change of
  its "Does what" fields or by "Load the list of actions" - never as a screen is shown
  or a policy viewed or focused (a built-in policy included), and waits for it at most
  10 s. A caller without `admin:GetPolicyCatalogue` (`read-write`, `read-only`) causes no
  audited refusal by looking. A save waits for the catalogue's answer, then checks the
  exact text it sends.
- **A placeholder `{tag_key}`** (9.5). A tag key may hold `/` (`object.ValidateTagKey`),
  so the template of a tag definition is `arn:cactusfs:admin:::tag-definition/{tag_key}`,
  whose placeholder takes `/` and no wildcard; `tag-definition/team/x` is the definition
  of the key `team/x`. The catalogue's templates have four placeholders, not three.
- **Wildcards refused in `{bucket}` and `{name}`** (9.5, `iam.FitsTemplate`). A name is
  one bucket or one entity, never a pattern: `{bucket}` and `{name}` hold no `/`, `*` or
  `?`, so `arn:aws:s3:::*` is no bucket and `user/*` no user, and an explanation about
  either is a 400. `{key}` takes any object key as it is, `*` and `?` included, since S3
  keys may hold them and the object use cases ask about them literally.
- **The explanation's record is detached** (2026-11-18, found by the review of 9.10).
  Decision 3, step 7, appends the record before the answer. A closed connection ends the
  request's context, and the replicated store takes nothing on an ended context, so a
  question authorized and answered could have left no record. `ExplainService.record`
  appends on `context.WithoutCancel` of the request's context, bounded by
  `auditWriteTimeout`, as `Auditor.write` does
  (`TestAnExplanationIsRecordedWhenItsCallerGoes`). The answer is still not given
  without its record. The console never aborts a question it sent.
- **The order of `ExplainService.Explain`** (9.5). Decision 3 lists authorization of
  the subject, then of the policies, then the subject, then the request. As built the
  steps run: the shape of the subject (a 400 before the gate, not recorded) → the gate,
  `admin:ExplainAccess` on the subject → the request read against the catalogue (a 400
  here is recorded) → the policies the answer depends on readable (403) → the subject
  resolved (404, recorded) → the decision → citations filtered → the record. So a
  request whose subject is not there and whose action is bad is answered 400, not 404,
  and that 400 is recorded even when the caller lacks `s3:GetBucketPolicy`; it tells
  nothing of a policy, since no decision is reached. What the code guarantees is no
  answer without a record: a question that cannot be decided (store unavailable, caller
  gone before the decision) gets neither.
- **The explainer is separated by structure, not only by type** (9.4). Decision 3 has
  `ports.AccessExplainer` implemented by `app.Authorizer`. The review found that a
  type-based rule let holders through and that an adapter could assert its authorizer to
  the port, so `app.Authorizer` has no method `Explain` and is no `AccessExplainer`: the
  one production implementation is the unexported `explainer` made by
  `app.NewExplainer(*Authorizer)` (`internal/app/explainer.go`; `porttest.Explainer`
  serves use-case tests), which calls the authorizer's own `decide`. It is made in
  one place, the `ExplainServiceConfig` of `cmd/cactusfs/node.go`.
  `TestOnlyTheExplainUseCaseHoldsAnExplainer` covers the source of `internal/app`,
  `internal/adapters` and `cmd/cactusfs`, not `internal/app` and the wiring alone: it
  refuses any other reference to the port, to `app.NewExplainer`, to `decide`, or a call
  of `Explain` on a type with the port's method set; `TestTheExplainerRuleSeesAHolder`
  plants the escapes it must catch.

Also, smaller: validation and explanation answer every refusal with one fixed text and
never return, record or log what a store or parser said (9.5); `ExplainedSubject`,
`ExplainedRequest`, `KeyPolicyVerdict`, `ExplanationAssumptions`, `PolicyLimits`,
`PolicyPrincipalForms` and `DecisionSource` are named components of the contract where
this record writes them inline, with the same wire shape (9.6); an empty optional field
the contract bounds below by 1 is a 400, not taken as absent (9.6); the "Who may do
this?" screen and the link "What may *name* do?" from a user's page (9.10).

**Open, for later steps** (not decisions of the owner; none blocks this record):

- The analysis has no resources field: the warning before a bucket is opened to anyone
  quotes each statement's Resource as written. Giving the resources is a contract
  addition.
- The rules of what a sub-bucket's policy is offered - its actions, keys under
  `<sub>/`, the bucket for listing, anyone only to a Deny - are constants of the console
  that only narrow what is offered (the node's validation and put decide). They could
  become a field of the catalogue (for instance `sub_bucket_policy_principals`, with the
  effects allowed to anyone).
- A link from a row of the API keys to "Who may do this?" with `?access_key_id=`, as
  the user's page has `?user=`.
- A check in a real browser, at desktop and phone width, and with a screen reader: the
  console's accessibility is tested in jsdom only. That is PLAN 9.13's.
- A test that ties the console's use of the catalogue's resource kinds (`bucket`,
  `object`) and the order of their placeholders to `iam.ResourceKinds()`: the console's
  tests use catalogues written by hand (the review of milestone 9).
- A grant of `admin:ExplainAccess` on the bare `arn:cactusfs:admin:::user` - a template
  the catalogue lists for the action - tells its holder an access key that is there
  (403) from one that is not (404), as `DeleteAPIKey`'s gate does; key IDs are random,
  so it confirms a known ID and lists none (the review of milestone 9).
- Question 71 of the threat model: the authorizer and the admin API's 5xx path log a
  damaged stored policy's parser text to the node's log (older than this record).

## Alternatives

- **The catalogue as a file of the console** - it would be React's word on which actions
  exist, and stale on every node of another release.
- **Validation by a dry run of the put** (a header or a query on the put) - a put
  authorizes, reads the bucket and audits; a validation must do none of the last two,
  and a flag that turns a put into no put is one mistake from a put.
- **One validation endpoint with a `kind`** - the authorization differs by kind, and
  each place already has a path.
- **A new action for validation** - one more grant for the same people; whoever may
  save may check.
- **The explanation reckoned in the console** from the policies it may read - a second
  authorizer, and wrong whenever a policy it may not read decides.
- **`admin:GetAccessExplanation`** - it would fall under the auditor's `admin:Get*` and
  disclose everybody's permissions to every auditor (open question 61).
- **Simulating at another time** - `aws:CurrentTime` would be judged at a time the node
  has not reached; the answer would be a guess, labelled as the authorizer's.
- **Group and role principals in bucket policies** (the first cut of 9.3) - a change of
  the language that older nodes would refuse (open question 60).

## Open, for the owner

Defaults are applied until answered; the threat model has each, numbered 60 to 70.

1. **Group or role principals in bucket policies** (60). Default: not built; users and
   anyone, as the language has them.
2. **Who holds `admin:ExplainAccess`** (61). Default: `cluster-admin` only.
3. **A user asking about their own access** (62). Default: no; the action is needed.
4. **A cited policy the caller may not read** (63). Default: hidden, name included.
5. **Every explanation audited** (64). Default: yes, before the answer is given.
6. **Validation authorized by the put's action** (65). Default: yes; no new action;
   `Idempotency-Key` accepted and ignored; CSRF needed.
7. **Ownership of a version in an explanation** (66). Default: not considered, and
   said.
8. **`NotAction`, `NotResource` and `NotPrincipal`** (67). Default: still refused.
9. **All four kinds of policy in the builder** (68). Default: yes.
10. **Milestone 9 and CLAUDE.md section 8** (69). Default: the plan runs without the
    amendment.
11. **What an explanation may tell of policies, and grants that gain it** (70).
    Default: an answer about a bucket needs `s3:GetBucketPolicy`, and one that could be
    in a sub-bucket `s3:GetSubBucketPolicy`, besides `admin:ExplainAccess`; a damaged
    policy is answered as a refusal; a wildcard grant on `user/*` that gains
    `admin:ExplainAccess` on upgrade keeps it, and the README says so where it describes
    "Who may do this?" (PLAN 9.12).
