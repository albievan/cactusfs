# ADR-0028: Previews of objects in the console

- **Status:** accepted (Albie, 2026-10-15: "Accept all defaults, and yes to 1, 2 and
  3"); implemented - the server, the console and the tests of PLAN Goal O, O.0 to
  O.12; what remains is O.14 (review, a real browser, roll-out)
- **Date:** 2026-10-15
- **Amended:** 2026-10-16 (PLAN O.13) - this record now says what was built. The
  decision is unchanged; where the first text said something the code does otherwise,
  the text was corrected and the section [Amended 2026-10-16](#amended-2026-10-16-as-built)
  says what it said before, what it says now and which code it was checked against.
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
  `SessionLocked`; 429 `TooManyRequests` with `Retry-After: 1`. The 409 is a mapping of
  this operation only: every other operation keeps 400 `InvalidArgument` for an SSE-C
  object. `HEAD` of the path is 405, as of a download.
- **A client that stops reading** (amended 2026-10-16): the listener's whole-response
  deadline is lifted for a preview as for a download, and replaced by one that moves
  with progress - each write must be taken within `adminapi.DefaultPreviewStall`
  (2 minutes). A write that is not ends the transfer, which closes the body and gives
  the preview's place back; a slow client that keeps reading is never cut off.

The use case, `ObjectService.Preview` (`internal/app/preview.go`; amended 2026-10-16):

1. authorizes `s3:GetObject` (or `s3:GetObjectVersion`) and resolves the version;
2. authorizes `admin:PreviewObject` with `ObjectCreator` = that version's `CreatedBy`;
3. **before any shard is read**, refuses, in this order: SSE-C (409); a stored type
   that names no kind (`preview.Expected`: 415); and - but for the text kinds - an
   object over `preview-max-size` (413). So 415 is said before 413;
4. takes a place among the previews being read (2 per caller, 16 per node; else 429),
   which the body's `Close` gives back - or the use case itself, on any failure before
   the body is handed out, a panic included;
5. opens exactly the version that was judged - by its ID, never "the latest" again;
   when its data was replaced or removed underneath, the whole judgement starts again,
   who made what is there now included (at most 4 times, as `Get` does);
6. reads the first `preview.HeadSize` (4 KiB) and classifies them (below); for an image
   whose head is full, reads on to at most `preview.BoundsHeadSize` (256 KiB) and
   checks its size there; then streams the object - for the text kinds its first 1 MiB,
   marked truncated when there is more. A GIF is streamed through `preview.GIFFrames`,
   which walks **every** frame to the trailer: at a frame that reaches past the screen
   (`ErrPreviewTooLarge`), or a block that is no GIF block (`ErrNotPreviewable`), the
   body fails (`checkedReader` drops the read that holds the bad frame) and the server
   ends the connection short of its `Content-Length`; the browser's fetch fails the
   broken body, and the dialog shows the preview as cut off - an error, never a
   shorter image.

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
  refused. *Amended 2026-10-16:* a GIF/JS polyglot that is also a whole GIF - its
  script inside a comment extension, a frame within the screen, a trailer - passes
  every check and **is** shown, stored as `image/gif` or untyped: as every byte in an
  `application/octet-stream` attachment, drawn by the console only from a blob typed
  `image/gif` (`previewtest.Cases`, `TestPreviewOfMaliciousFiles` in the domain, the use
  case and the API; `web/src/preview/malicious.test.ts`). Stored as script, as text or
  as PNG it is refused (415). Text whose first bytes are a binary signature (`GIF89a…`,
  `%PDF-…`) is refused as text too.
- The kinds are part of the contract (`X-CactusFS-Preview-Kind`): added to, never
  changed.

### Caps and concurrency

| Bound | Value |
|---|---|
| Size of an object previewed | setting `preview-max-size` (whole MiB, the same on every node), 32 MiB by default, 1 MiB to 256 MiB; over it 413, before a shard is read - but for the text kinds, which are never refused for their size |
| Text, CSV, JSON | the first 1 MiB, `X-CactusFS-Preview-Truncated: true` when there is more |
| An image | at most 16 384 px a side and 40 MP, read from its header (PNG `IHDR`, JPEG frame header, WebP `VP8 `/`VP8L`/`VP8X`; for a GIF its screen, and every frame that begins in the bytes looked at must lie within the screen); over it 413. *Amended 2026-10-16:* every later frame of a GIF is checked against the screen as the body streams (`preview.GIFFrames`); one past it cuts the transfer off |
| Where an image's size is looked for | its first 256 KiB: a JPEG may carry metadata before its frame header; an image whose size is not found there, or is zero - a GIF with no frame beginning there, or with a malformed block - is 415 |
| Previews at once | 2 per principal (counted by the audit trail's actor), 16 per node; beyond, 429 with `Retry-After: 1` |
| A client that stops reading | each write taken within 2 minutes (`adminapi.DefaultPreviewStall`), else the transfer ends and the place is given back (amended 2026-10-16) |
| What the console reads of an answer | at most 256 MiB (the largest `preview-max-size`), said by `Content-Length` or found while reading; past it the console stops and says so (amended 2026-10-16) |

(Answer 8.) An image's size is checked because a few bytes of header can make a browser
allocate gigabytes; the caps on bytes bound the gateway, the network and the worker.

### The renderers and their bounds

The console fetches the bytes with the generated client, chooses a renderer by
`X-CactusFS-Preview-Kind` only, and draws them itself. Nothing of an object is ever
navigated to, framed or inserted as markup. *Amended 2026-10-16, as built:* "Preview"
is offered in an object's opened row for the stored types the contract names a kind
for, and for none or `application/octet-stream`; it previews the **current** version
(the console sends no `version_id`); the body is read through a counting stream into
one Blob, at most 256 MiB, aborted when the dialog closes; a body that ends early is an
error, never a shorter file.

- **Images**: a blob URL in an `<img>`, revoked when the dialog closes. Never SVG: the
  Blob is re-typed as the kind's own raster type (`image/png`, `image/jpeg`,
  `image/gif`, `image/webp`), never as anything the object was stored as.
- **Text, JSON**: text nodes, decoded by a non-fatal `TextDecoder` and not
  re-formatted. **CSV**: our own RFC 4180 reader, a table of the first 1 000 rows and
  100 columns, cells as text.
- **PDF** (amended 2026-10-16, as built): `pdfjs-dist` 6.4.299 (Apache-2.0), loaded by
  dynamic `import()` only for a PDF, in its own chunk. The console makes pdf.js's worker
  itself - `new Worker(<pdf.worker.min.mjs of the console's origin>, { type: 'module' })`,
  given to `PDFWorker.create({ port })` - so pdf.js never falls back to parsing on the
  page's thread; closing terminates that thread first. `getDocument` is given the bytes
  as data and `useWasm: false`, `enableXfa: false`, `maxImageSize` 40 MP, `verbosity` 0,
  and `enableScripting: false` and `isEvalSupported: false` - which 6.4 no longer reads
  (it has no `eval`, and scripting belongs to its viewer's annotation layer, which the
  console does not use) and are passed so that a later release which reads them finds
  them off; no `url`, `range`, `cMapUrl`, `standardFontDataUrl`, `iccUrl` or
  `docBaseUrl`. `wasmUrl` names `assets/pdfjs-6.4.299/` of the console's origin, which
  holds pdf.js's two non-wasm fallback decoders, `jbig2_nowasm_fallback.js` (JBIG2 and
  CCITT, scanned pages) and `openjpeg_nowasm_fallback.js` (JPEG 2000) - wasm2js output,
  plain JavaScript, no `eval`, no request - which pdf.js imports when it does not use
  WebAssembly. No bundled fonts, character maps, colour profiles, wasm or `pdf.sandbox`
  in `dist`: `pdfjsDistProblems` checks the bundle and the output directory, and a
  build that fails it removes its output. A page is drawn into a canvas with
  `annotationMode: ENABLE` - what annotations look like, as pixels - and no annotation,
  form or text layer exists, so no link is followed and no action runs. One page at a
  time, on demand; a page's canvas has at most 16 MP and 16 384 px a side - a larger
  page is drawn smaller, not refused. A browser that lacks one of the fifteen built-ins
  pdf.js 6.4 uses without polyfilling them is told it is too old, and nothing of pdf.js
  is loaded.
- **DOCX, XLSX** (amended 2026-10-16, as built): readers of our own in a Web Worker -
  no OOXML is parsed on the server (answer 5), and none on the page. The worker is a
  module of the console's origin (`ooxml.worker.ts`, its own chunk), one per document,
  ended once it answers, at once when the preview closes, and after 10 s
  (`OOXML_TIMEOUT_MS`); a worker that cannot load is said as such and nothing is ever
  read on the page's thread instead. The zip is read from its central directory by
  `web/src/preview/zip.ts`; only the parts the readers need are inflated, by `fflate`'s
  (MIT) synchronous streaming `Inflate` fed 8 KiB at a time - never its `unzipSync`,
  which trusts what an entry says, nor its async API, which makes workers from blob
  addresses. What is unpacked is counted as it comes (`OOXML_BOUNDS`: 64 MiB in all,
  32 MiB a part, 100 times a part's compressed size, 10 000 entries - the last judged
  from the end record before any entry is read); zip64, encryption, methods other than
  stored and deflate, overlapping entries and names that differ only by case are
  refused. A dedicated worker has no `DOMParser`, so the parts are read by a small,
  strict XML reader of our own (`web/src/preview/xml.ts`): no DTD, the five entities of
  XML and character references only, nesting at most 256, at most 64 namespace
  declarations on an element and 256 in scope; `<!DOCTYPE` or `<!ENTITY` anywhere in a
  part, in any case, is refused as unsafe before a character of it is read - by each
  reader and again by the XML reader. DOCX: paragraphs, headings, lists, tables (to 8
  deep), emphasis, line breaks and embedded PNG, JPEG and GIF judged by their signature
  and size as the API judges an image (a GIF walked to its trailer, at most 1 000
  frames), at most 10 000 blocks, 50 000 nodes, 1 000 000 characters, 100 images and
  100 MP of images; a linked image is never fetched. No page layout, styles or charts.
  XLSX: at most 20 sheets of 1 000 rows by 100 columns, 1 000 000 characters in all and
  32 767 a cell, shared, inline and rich strings, the cached values of formulas (never
  read or evaluated), dates in the 1900 and 1904 systems. **The page does not take the
  worker's word**: `web/src/preview/ooxml-check.ts` walks the answer without recursion
  and refuses, as damaged, a model of another shape or past any of those bounds before
  anything is drawn; a model that still cannot be drawn is caught by an error
  boundary, which says so in its place and keeps the dialog. The result is React
  elements and text nodes; what is not shown is said.
- An eslint rule (`web/eslint.config.js`, amended 2026-10-16 to the list as built)
  refuses `innerHTML`, `outerHTML`, `insertAdjacentHTML`, `srcdoc`/`srcDoc`,
  `setHTMLUnsafe`, `parseHTMLUnsafe`, `createContextualFragment` and
  `dangerouslySetInnerHTML` as a JSX attribute, an object key or a string naming them,
  `document.write`/`writeln`, and giving the document another name;
  `markup-rule.test.ts` lints every case.

docx-preview, mammoth and exceljs depend on jszip, licensed `MIT OR GPL-3.0-or-later`,
which `make licences` refuses as written; SheetJS on npm is from 2022 and has known
vulnerabilities. Our readers show less, and nothing of theirs can run.

### The console's CSP

`img-src 'self' data: blob:` and an explicit `worker-src 'self'`. Nothing else changes:
no `unsafe-eval`, `unsafe-inline` or `wasm-unsafe-eval`. As built
(`internal/adapters/console/console.go`, asserted exactly by
`TestConsolePolicyLoosensNothing`):

```
default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:;
connect-src 'self'; font-src 'self'; worker-src 'self'; form-action 'self';
base-uri 'none'; frame-ancestors 'none'
```

Both workers - pdf.js's and the document reader's - are module scripts of the console's
own origin, so `worker-src 'self'` admits them and refuses a worker made from a `blob:`
address. The console adapter types every file the build emits itself (`.js` and `.mjs`
as `text/javascript`), not by the host's table of types, so that `nosniff` never makes
a browser refuse a worker on Windows.

### Counted as a download

A preview is a download (ADR-0016): counted once, for the caller, when its body is
closed - however much of it was read - as a download through the management API is. An
allowed preview is not audited, as a download is not (answer 9); a refused one is, as
every denial is: the one `ports.Authorizer` writes the refusal of `s3:GetObject` or of
`admin:PreviewObject`, folded per node like every refusal (`app.Auditor`): the first
refusal of one actor, action and address is written at once; repeats of it within
10 s are counted into one more record when the window ends; past 64 kinds of refusal
in a window, the rest are counted into a suppressed count. A 413, 415 or 409 is no
denial and is not audited.

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
  works as before on both releases. *Amended 2026-10-16, as built:* the path it does not
  know is answered `404` with the `code` `NotFound` ("No such endpoint"), and the
  console's preview dialog words every 404 alike - "The object is not there any more:
  it was deleted, or its bucket was." - which is wrong for this case. During a rolling
  upgrade behind a balancer, a preview that reaches a node of the previous release says
  that, and asking again may reach a new node. Not changed in O.13, which changes no
  behaviour; the cure (a 404 whose `code` is `NotFound` worded as "previews are not
  available on the node that answered (an older release); try again") is part of
  O.14's criterion, to be done before its roll-out.
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
- ~~**Residual risk: the frames of a GIF** that begin after its first 256 KiB are not
  checked against its screen. A later frame that claims up to 65 535 × 65 535 pixels can
  make the browser that shows the preview fail or stall; the gateway and every other
  user are not affected. Walking every frame of the whole object would remove it at the
  cost of reading it twice.~~ *Closed (PLAN O.5; amended 2026-10-16):* the body of a GIF
  is streamed through `preview.GIFFrames`, which walks every frame to the trailer as
  the bytes go by - no second read - and fails the transfer at a frame past the screen
  (`TestPreviewCutsOffAGIFFramePastItsScreen` in `internal/app`,
  `TestAGIFFramePastItsScreenCutsThePreviewOff` in `adminapi`). The bytes before that
  frame have reached the browser, but the server ends the connection short of its
  `Content-Length`: the browser's fetch fails the broken body, and the dialog shows the
  preview as cut off and draws nothing of it.
- **Residual risk: a slow reader holds a place.** A client that keeps taking bytes,
  however slowly, keeps its place among the 16 of a node for as long as the transfer
  lasts (one that stops for 2 minutes loses it). Two places per caller bound what one
  caller holds; many callers together can hold all 16, and previews on that node then
  answer 429 until one ends. Downloads and every other request are not affected.
- **Residual risk: the memory of a PDF preview.** A PDF is read whole into the browser
  before pdf.js sees it - up to `preview-max-size`, 256 MiB at most - and held once in
  the dialog's Blob and once in pdf.js's worker. Inside it, a page may hold any number of
  images (each at most 40 MP, `maxImageSize`) and streams compressed many thousand to one
  (a flate bomb); pdf.js bounds neither. Such a PDF can make the worker or the tab that
  shows it run out of memory or stall; closing the dialog ends the worker. As with the GIF,
  the gateway and every other user are not affected.
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

As built (amended 2026-10-16):

| Claim | Test |
|---|---|
| A refusal by type before any byte is read; every GIF frame to the trailer | `TestExpected`, `TestExpectedAgreesWithClassify`, `TestGIFFrames`, `TestGIFFramesFailsForGood`, `TestGIFFramesProperties`, `FuzzGIFFrames` (`internal/domain/preview`) |
| Who may, every refusal, the version shown, counted as a download, places at once, data gone underneath | `TestPreviewWhoMay`, `TestPreviewOwnsTheVersionShown`, `TestPreviewRefusals`, `TestPreviewKinds`, `TestPreviewIsADownload`, `TestPreviewsAtOnce`, `TestPreviewOfDataThatIsGone`, `TestPreviewJudgesAgainWhatReplacedTheVersion`, `TestAPreviewThatPanicsGivesItsPlaceBack` (`internal/app`) |
| The headers, the problems, the stall, a session, 429 | `TestPreview`, `TestPreviewProblems`, `TestTooManyPreviews`, `TestPreviewInAConsoleSession`, `TestAStalledPreviewGivesItsPlaceBack`, `TestASlowPreviewIsNoStalledOne` (`adminapi`) |
| Hostile files (34 cases made by code, `previewtest.Cases`) | `TestMaliciousFiles` (`internal/domain/preview`), `TestPreviewOfMaliciousFiles` (`internal/app`, `adminapi`), `TestAPreviewOverTheCapReadsNothing` (`adminapi`); the console's `web/src/preview/malicious.test.ts` |
| The console's CSP, exactly | `TestConsolePolicyLoosensNothing` (`internal/adapters/console`) |
| The renderers and their bounds | `web/src/preview/{preview,pdf,office}.test.tsx`, `{xml,zip,docx,xlsx,ooxml,pdfjs-dist-check,markup-rule}.test.ts` |
| Four nodes: owner through another node, download without preview refused and audited, assignment, a Deny, a narrowed key, the counts; a version without a maker | `TestPreviewAcrossTheCluster`, `TestPreviewOfAVersionWithoutAMaker` (`test/integration`) |

## Amended 2026-10-16 (as built)

PLAN O.13 checked every claim above against the code shipped by O.0 to O.12. The
decision - who may, the endpoint, the type decision, the renderers, the CSP, the caps,
the rolling upgrade - is unchanged. What the first text of 2026-10-15 said, and what it
says now:

| Was | Is | Checked against |
|---|---|---|
| The DOCX/XLSX worker refuses `<!DOCTYPE` and `<!ENTITY` "before `DOMParser` sees a part" | No `DOMParser` is used anywhere: a dedicated worker has none. The parts are read by our own strict XML reader, the zip by our own central-directory reader with `fflate`'s streaming `Inflate`; the check of the worker's answer on the page was added | `web/src/preview/{xml,zip,opc,ooxml,ooxml.worker,ooxml-check,office}.ts` |
| "its worker from the same origin" (pdf.js) | The console makes the module worker itself and hands it to `PDFWorker.create`; pdf.js 6.4.299 and every option it is given, the two fallback decoders at `wasmUrl`, the browser check, the `dist` check, `annotationMode: ENABLE` on a canvas | `web/src/preview/{pdf.ts,pdfjs-assets.ts,pdfjs-dist-check.ts,PdfPreview.tsx}`, `web/package.json` |
| "at most 16 MP a page" | A page's canvas has at most 16 MP and 16 384 px a side; a larger page is drawn smaller, not refused | `pageSize` in `web/src/preview/pdf.ts` |
| SSE-C and the size cap are refused before a shard is read | Also a stored type of no kind (415), and in the order SSE-C, type, size; text kinds are never refused for size; the place among previews is taken after the judgement and before a shard is opened | `previewTarget`, `previewOnce` in `internal/app/preview.go` |
| GIF frames after the first 256 KiB are a residual risk | Closed: `preview.GIFFrames` checks every frame as the body streams | `internal/domain/preview/stream.go`, `previewBodyOf` |
| "the usual GIF/JS polyglot … is refused (415)" | One that is a whole GIF is shown as a GIF, from a blob typed `image/gif`, in an octet-stream attachment | `previewtest.Cases` and the three suites of O.11 |
| 429 "with `Retry-After`"; nothing on stalled clients | `Retry-After: 1`; a write not taken within 2 minutes ends the transfer and gives the place back; `HEAD` is 405 | `internal/adapters/adminapi/handler.go` (`DefaultPreviewStall`, `newProgressWriter`, `fail`) |
| The eslint rule names six sinks | It names eight sinks and `document.write`/`writeln`, in every spelling a rule of syntax can see | `web/eslint.config.js`, `web/src/preview/markup-rule.test.ts` |
| A node of the previous release: "the console says that previews are not available there" | It answers 404 `NotFound`, which the console words as an object that is gone; follow-up proposed for O.14 | `handler.go` (the catch-all of `BasePath`), `refusalOf` in `web/src/preview/PreviewDialog.tsx` |
| A refusal is audited | By the one authorizer, folded per node (the first at once, repeats of one actor, action and address within 10 s as one more record, past 64 kinds a window a suppressed count); 409, 413 and 415 are no denials and are not audited | `internal/app/audit.go`, `test/integration/preview_test.go` |
| (not said) | The console previews the current version only; it reads at most 256 MiB of an answer | `openPreview`, `PREVIEW_MAX_BYTES` in `web/src/preview/fetch.ts` |

Every claim of the first text not in this table was found as built: the owner rule
(`owns` and `Decide` in `internal/domain/iam/iam.go`), the built-in roles
(`internal/domain/iam/builtin.go`), the kinds and signatures (`kindOf`, `signature`,
`Classify`), the image bounds and the 256 KiB head (`ImageBounds`, `BoundsHeadSize`),
the headers of a 200 (`PreviewObject` in `internal/adapters/adminapi/operations.go` and
the headers every answer of the API carries: `nosniff`, `no-store`), the 32 MiB default
and its range (`app.DefaultPreviewMaxSize`, `MinPreviewMaxSize`, `MaxPreviewMaxSize`),
the 1 MiB of text (`app.PreviewTextSize`), 2 and 16 at once (`PreviewsPerPrincipal`,
`PreviewsPerNode`), the CSP (`internal/adapters/console/console.go`), the zip bounds
and the 10 s (`OOXML_BOUNDS`, `OOXML_TIMEOUT_MS`), and the sentinels.
