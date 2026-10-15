# ADR-0028: Previews of objects in the console

- **Status:** accepted (Albie, 2026-10-15: "Accept all defaults, and yes to 1, 2 and
  3"); being implemented (PLAN Goal O, O.0 to O.14)
- **Date:** 2026-10-15
- **Builds on:** ADR-0006 (identity and access), ADR-0007 (management API and console),
  ADR-0016 (who uploaded and who read an object), ADR-0011 (bucket policies)
- **Refines:** ADR-0006, "a user may do what some policy of theirs allows": ownership of
  an object version becomes a source of an Allow, for one action

## Context

Albie, 2026-10-15: "Add the functionality to be able to preview known object types e.g.
image file, pdf, docx, excel etc. The user must own or have preview assigned to them."

What the code had before this ADR:

- An object can be downloaded through S3 and through the management API
  (`s3:GetObject`, `s3:GetObjectVersion`). The console can only offer the download; it
  shows nothing of what an object holds.
- A version records who committed it, `object.Version.CreatedBy`, since entry schema 3
  (ADR-0016), in the words the audit trail uses for actors (`actorOf`, `internal/app`):
  `user/<name>` for a user, by a session or by one of the user's keys; `root` for the
  root identity; `key/<access key ID>` for a key that belongs to no user;
  `service/<name>` and `node/<ID>` for what the deployment does by itself; `anonymous`
  for a caller without credentials, where a bucket policy lets one write. Versions
  written before schema 3 have none. Of all these forms only `user/<name>` can name an
  owner (below).
- The one `ports.Authorizer` decides every request with `iam.Decide`. Stored policies
  are parsed strictly again whenever they are read: a field, an operator or a condition
  key a release does not know makes the document unreadable to it.
- The console is served under a Content Security Policy that allows script, style,
  images and calls of its own origin only (`img-src 'self' data:`).

Showing an object in a browser is not like downloading it. The bytes of an object are
whatever its uploader made them, and so is its stored type: an "image" can be HTML, a
PNG can carry a script, a few bytes of header can claim billions of pixels, a DOCX is a
zip that can unpack to gigabytes of XML with entities in it. Whatever the console shows
must not run, must not reach the console's origin as a document, and must not take the
browser down.

## Decision

### Who may preview

A new admin action, `admin:PreviewObject`, asked on the object
(`arn:aws:s3:::<bucket>/<key>`).

A caller may preview the version shown when **both** hold:

1. the caller is allowed `s3:GetObject` on the object - `s3:GetObjectVersion` when a
   version is named. **A preview is never more than a download** (Albie's answer 1).
2. the caller is allowed `admin:PreviewObject` on it, **or** owns the version: the
   version's `CreatedBy` is `user/<the caller's user name>` (answer 2: the owner is the
   user who committed the version shown, not the first uploader of the key, not the
   bucket's creator).

and nothing refuses it:

- An explicit Deny of `admin:PreviewObject`, in any policy that applies, refuses the
  owner as well (answer 3). Ownership turns the policies' *silence* into an Allow, and
  nothing else.
- An API key's inline policy must allow both actions, for the owner too: a key never
  exceeds its owner, and never exceeds itself.
- A locked session (ADR-0027) previews nothing: it may store objects only.
- The root identity may, as it may everything.
- Anonymous callers never may: the management API authenticates every request, and no
  bucket policy can name an `admin:` action (ADR-0011).
- Nothing but a user owns a version. `root`, `key/…`, `service/…`, `node/…` and
  `anonymous` creators make no owner; a version without `CreatedBy` has none and is
  previewed only by assignment.

**Why an admin action.** An `s3:` action would be granted by `s3:*`, which the built-in
`bucket-admin` and many written policies hold, and could be opened to anyone by a bucket
policy. **Why not `admin:Get…`.** The built-in `auditor` holds `admin:Get*` and
`admin:List*` to see how the deployment is run without reading any object; a preview is
an object's content. `admin:PreviewObject` begins with neither.

**Where the rule sits.** In `iam.Decide`, through a field of the request that lives in
memory only, `iam.Request.ObjectCreator`, which the use case sets from the version it
has resolved - never from anything the caller sent. For `admin:PreviewObject` only,
when `ObjectCreator == "user/" + Principal.User` and the user's policies and the bucket's
are silent, the request counts as allowed; then the key's inline policy is asked as for
every request. Every other action ignores `ObjectCreator`. A condition key
(`cactusfs:ObjectCreator`) was the alternative: a policy that used it would be
unreadable on a node of the previous release during a rolling upgrade, and with it every
other statement of that policy.

**Built-in roles.** `bucket-admin`, `read-write` and `read-only` gain
`admin:PreviewObject` (answer 4); `cluster-admin` holds it through `admin:*`; `auditor`
does not. Built-in policies are part of the binary, so this needs no change of state.

### The endpoint

`GET /api/v1/buckets/{bucket}/object/preview?key=<key>[&version_id=<id>]`,
`operationId: previewObject`, `x-cactusfs-action: admin:PreviewObject` - described in
`api/openapi/admin.yaml` as also needing `s3:GetObject` - defined in the contract before
it is implemented.

- **200** `application/octet-stream`, always, whatever the object is, with
  `Content-Disposition: attachment`, `X-Content-Type-Options: nosniff`,
  `Content-Security-Policy: sandbox; default-src 'none'`,
  `Cross-Origin-Resource-Policy: same-origin`, `Cache-Control: no-store`,
  `Content-Length`, `ETag`, `X-CactusFS-Preview-Kind: <kind>` and
  `X-CactusFS-Preview-Truncated: true|false`. No `Range`. Should anything ever navigate
  to the URL, the browser saves a file; it never renders one on the console's origin.
- **Problems** (`application/problem+json`): 403 `AccessDenied`; 404 `NoSuchBucket`,
  `NoSuchObject`, `NoSuchVersion`; 409 `CustomerKeyRequired` for an SSE-C object - the
  console holds no customer key; 413 `PreviewTooLarge`; 415 `NotPreviewable`; 423
  `SessionLocked`; 429 `TooManyRequests` with `Retry-After`.

The use case, `ObjectService.Preview`:

1. authorizes `s3:GetObject` (or `s3:GetObjectVersion`) and resolves the version;
2. authorizes `admin:PreviewObject` with `ObjectCreator` = that version's `CreatedBy`;
3. refuses SSE-C, and an object over the cap, **before any shard is read**;
4. opens exactly the version that was judged - by its ID, never "the latest" again;
5. reads the first `preview.HeadSize` (4 KiB) and classifies them (below); for an image
   reads on to at most `preview.BoundsHeadSize` (256 KiB) and checks its size there;
   then streams the object - for the text kinds its first 1 MiB, marked truncated when
   there is more.

### What kind of thing an object is

Decided on the server by `internal/domain/preview` (standard library only) from the
**stored type AND the bytes' signature**, never from the key or its extension; the
stored type comes from the uploader and can lie (answer 7).

| Kind | Stored type (lower-cased, parameters dropped) | The bytes must begin with |
|---|---|---|
| `png` | `image/png` | `\x89PNG\r\n\x1a\n` |
| `jpeg` | `image/jpeg`, `image/jpg` | `FF D8 FF` |
| `gif` | `image/gif` | `GIF87a` or `GIF89a` |
| `webp` | `image/webp` | `RIFF` …. `WEBP` |
| `pdf` | `application/pdf` | `%PDF-` at offset 0 |
| `docx` | `application/vnd.openxmlformats-officedocument.wordprocessingml.document` | `PK\x03\x04` |
| `xlsx` | `application/vnd.openxmlformats-officedocument.spreadsheetml.sheet` | `PK\x03\x04` |
| `text`, `csv`, `json` | `text/plain`, `text/csv`, `application/json` | valid UTF-8 (a character cut off at the end of the 4 KiB allowed), no NUL, and no signature of a binary kind |

- `application/octet-stream`, or no stored type, lets the bytes decide - for images and
  PDF only. A zip says nothing of what it holds: a DOCX stored as octet-stream is not
  previewed.
- **Never previewable**, whatever they claim or carry: SVG, HTML, XHTML, any `*xml`,
  any script type, and every type not in the table (PPTX, `.doc`, `.xls`, HEIC, TIFF,
  AVIF, audio, video). A polyglot - a GIF that is also a script - is a GIF if it is
  stored as one, and is then only ever drawn as an image from a blob - if its size
  check passes: a header that opens a comment and has no frame after it, as the usual
  GIF/JS polyglot does, is refused there (415). Stored as script or as text it is
  refused.
- The kinds are part of the contract (`X-CactusFS-Preview-Kind`): added to, never
  changed.

### Caps and concurrency

| Bound | Value |
|---|---|
| Size of an object previewed | setting `preview-max-size`, 32 MiB by default, 1 MiB to 256 MiB; over it 413, before a shard is read |
| Text, CSV, JSON | the first 1 MiB, `X-CactusFS-Preview-Truncated: true` when there is more |
| An image | at most 16 384 px a side and 40 MP, read from its header (PNG `IHDR`, JPEG frame header, WebP `VP8 `/`VP8L`/`VP8X`; for a GIF its screen, and every frame that begins in the bytes looked at must lie within the screen); over it 413 |
| Where an image's size is looked for | its first 256 KiB: a JPEG may carry metadata before its frame header; an image whose size is not found there, or is zero - a GIF with no frame beginning there, or with a malformed block - is 415 |
| Previews at once | 2 per principal, 16 per node; beyond, 429 with `Retry-After` |

(Answer 8.) An image's size is checked because a few bytes of header can make a browser
allocate gigabytes; the caps on bytes bound the gateway, the network and the worker.

### The renderers and their bounds

The console fetches the bytes with the generated client, chooses a renderer by
`X-CactusFS-Preview-Kind` only, and draws them itself. Nothing of an object is ever
navigated to, framed or inserted as markup.

- **Images**: a blob URL in an `<img>`, revoked when the dialog closes. Never SVG.
- **Text, JSON**: text nodes. **CSV**: a table of the first 1 000 rows, cells as text.
- **PDF**: `pdfjs-dist` (Apache-2.0), loaded by dynamic `import()` only for a PDF, its
  worker from the same origin; `enableScripting: false`, `useWasm: false`; no bundled
  fonts, character maps, colour profiles or wasm in `dist`; canvas only - no annotation,
  form or text layer, so no link is followed and no action runs; pages on demand, at most
  16 MP a page.
- **DOCX, XLSX**: small readers of our own on `fflate` (MIT) in a Web Worker - no OOXML
  is parsed on the server (answer 5). The worker counts what it unpacks (64 MiB in all,
  32 MiB a part, a ratio of 100:1, 10 000 entries) and refuses `<!DOCTYPE` and
  `<!ENTITY` before `DOMParser` sees a part. DOCX: paragraphs, headings, lists, tables,
  emphasis, line breaks and embedded PNG, JPEG and GIF - no page layout, styles or
  charts. XLSX: at most 20 sheets of 1 000 rows by 100 columns, shared strings, the
  cached values of formulas (never evaluated), dates. The worker is ended after 10 s.
  The result is React elements and text nodes; what is not shown is said.
- An eslint rule refuses `dangerouslySetInnerHTML`, `innerHTML`, `outerHTML`,
  `insertAdjacentHTML`, `srcdoc` and `document.write` in the console.

docx-preview, mammoth and exceljs depend on jszip, licensed `MIT OR GPL-3.0-or-later`,
which `make licences` refuses as written; SheetJS on npm is from 2022 and has known
vulnerabilities. Our readers show less, and nothing of theirs can run.

### The console's CSP

`img-src 'self' data: blob:` and an explicit `worker-src 'self'`. Nothing else changes:
no `unsafe-eval`, `unsafe-inline` or `wasm-unsafe-eval`.

### Counted as a download

A preview is a download (ADR-0016): counted once, for the caller, when its body is
closed, as a download through the management API is. An allowed preview is not audited,
as a download is not (answer 9); a refused one is, as every denial is.

### New sentinels

`domain.ErrNotPreviewable` (415 `NotPreviewable`) and `domain.ErrPreviewTooLarge` (413
`PreviewTooLarge`), neither a denial. They have the wire names `not_previewable` and
`preview_too_large`; a node of the previous release reads them as an unclassified
failure, which nothing between nodes depends on. The S3 API never meets them.

## Rolling upgrades and rollback

- **No change of the entry schema, of the state, of the on-disk format or of the crypto
  design.** `ObjectCreator` lives in memory; `CreatedBy` exists since schema 3.
- A node of the previous release does not serve the endpoint: it answers it as a path
  it does not know, and the console says that previews are not available there. Everything else
  works as before on both releases.
- A policy that names `admin:PreviewObject` is readable by the previous release - it
  accepts any `admin:<name>` - and grants nothing there.
- The built-in roles differ between the releases for one action that only the new
  release asks.
- **Rollback** is the previous binary: nothing is left behind to migrate or remove.
  Policies naming the action stay valid and inert.

## Consequences

- **Name reuse.** Ownership is by user name, as `CreatedBy` records it. A user deleted
  and created again under the same name owns what the first one committed. The remedy is
  a Deny of `admin:PreviewObject`, or not reusing names; the same holds of every
  user-name match in policies.
- **Versions from before schema 3** have no `CreatedBy`: nobody owns them, and they are
  previewed only by whoever holds the action.
- **A preview is not view-only protection.** Whoever may preview may download, and the
  bytes reach the browser either way. The rule decides who sees an object in the
  console, not who can keep a copy.
- **Ownership is of the version shown.** Whoever overwrites a key owns the new version,
  not the old ones.
- Who uploaded with the root identity or a key without a user owns nothing.
- Every preview is a full read of up to `preview-max-size` from the erasure set, and a
  download in the counts.
- **Residual risk: the frames of a GIF** that begin after its first 256 KiB are not
  checked against its screen. A later frame that claims up to 65 535 × 65 535 pixels can
  make the browser that shows the preview fail or stall; the gateway and every other
  user are not affected. Walking every frame of the whole object would remove it at the
  cost of reading it twice.
- Previews cover the listed kinds only; SSE-C objects cannot be previewed in the console.
- `iam.Decide` gains a second source of an Allow besides policies. It is confined to one
  action, tested for every other action to change nothing, and a Deny still wins.

## Alternatives considered

| Alternative | Why not |
|---|---|
| `s3:PreviewObject` | Granted by `s3:*` and openable by a bucket policy to anyone. |
| `admin:GetObjectPreview` | Held by the auditor through `admin:Get*`. |
| A condition key for the creator | Unreadable to the previous release during a rolling upgrade. |
| Ownership without `s3:GetObject` | A preview would then be more than a download. |
| Serving the object with its stored type for the browser to render | The stored type is the uploader's word: stored XSS on the console's origin. |
| Rendering on the server (thumbnails, PDF to images, OOXML to HTML) | Untrusted image, PDF, zip and XML parsing on the gateway, and dependencies outside §3. |
| docx-preview, mammoth, exceljs, SheetJS | Licence (jszip) and known vulnerabilities (SheetJS on npm). |

## Tests

| Claim | Test |
|---|---|
| The owner rule, every case of it | `TestPreviewByOwnership` (`internal/domain/iam`) |
| Every other action ignores `ObjectCreator` | `TestOwnershipGivesNothingElse` (`internal/domain/iam`) |
| The built-in roles | `TestBuiltInRoles` (`internal/domain/iam`) |
| The type decision, polyglots, HTML as PNG, PNG as HTML | `TestClassify`, `TestTheCaseAndParametersOfATypeChangeNothing`, `FuzzClassify` (`internal/domain/preview`) |
| Image bounds, a 65 535 × 65 535 PNG header | `TestImageBounds`, `FuzzImageBounds` (`internal/domain/preview`) |
| The sentinels have answers in both APIs | `TestEverySentinelIsInOneOfTheTwoLists` (`internal/domain`), `TestEverySentinelHasAnAnswer` (`adminapi`, `s3api`) |
| The use case, the handler, the CSP, the console, four nodes | PLAN O.5 to O.12 |
