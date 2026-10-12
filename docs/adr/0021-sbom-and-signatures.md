# ADR-0021: SBOMs and image signatures, made and verified offline

- **Status:** accepted - the procedure and its gate (PLAN 7.3) and the release that
  uses it (PLAN 7.9, decided by Albie on 2026-10-12: see "Releases: keyless" below).
  No release has been published yet
- **Date:** 2026-10-12
- **Milestone:** 7
- **Builds on:** the fresh-clone and air-gap rules (CLAUDE.md sections 2 and 3)

## Context

Whoever runs CactusFS should be able to tell that an image is the one that was
released and what is in it, on a network that reaches nothing but their own registry.
The usual "keyless" signing needs three services on the internet (an identity provider,
a certificate authority, a transparency log) at signing and, without extra work, at
verification. The registry path and the identity that signs a release are not decided:
they depend on the module path, which is still open (CLAUDE.md section 11). What can
be decided and proved now is the procedure.

*That was the state when the gate was written. The same day the owner decided the rest;
the sections up to "Consequences" describe the gate, which did not change, and
"Releases: keyless" describes what a release does with it.*

## Decision

- **SBOM.** SPDX 2.3 JSON, made by `syft`: one for the image of each architecture, read
  from the engine's image store as an archive (`docker image save`), and one for the
  console's packages, read from `web/pnpm-lock.yaml` alone. `make sbom` writes them to
  `dist/sbom/` and fails unless each is SPDX 2.3, names the Go module itself and
  `github.com/hashicorp/raft` (image) or `react` and `bootstrap` (console), and has a
  plausible number of packages. The image holds one static binary; what syft reads is
  the module list the Go linker embeds in it, which `-trimpath -s -w` does not remove.
- **Signature.** `cosign` with a **key pair**, no Fulcio, no Rekor, no network: signed
  with `--tlog-upload=false`, verified with `--key` and `--insecure-ignore-tlog=true`
  (the flag's name is cosign's; it means "there is no transparency-log entry to look
  for", which is the case by design). Always **by digest**, never by tag. Signed are
  the multi-arch manifest list and the manifest of each platform.
- **Attestation.** The SBOM of a platform image is attached to that image's digest as a
  signed in-toto attestation (`cosign attest --type spdxjson`), by the same key.
- **The gate.** `make sign-verify` (in `make check`) starts a throw-away registry,
  pushes both images, makes a key pair for the run, signs, attests and verifies, and
  proves five refusals: an image never signed; a signed tag moved to another image;
  another public key; one image's signature put on another image; one image's
  attestation put on another image. A refusal counts only when cosign reports the
  expected class of error - a tool that crashed or could not reach the registry fails
  the gate. cosign and syft run with a proxy that leads nowhere for every host but the
  registry, so a call to the internet fails the gate instead of passing unnoticed.
- **The dry run.** `scripts/release.sh --dry-run` does against that registry what a
  release does: manifest list, signatures, attestations, `dist/release/` with
  `SHA256SUMS` of the binaries, the SBOMs and the public key, and the commands to
  verify. With no argument the script refuses. The gate runs the dry run.
- **Tools.** cosign, syft and the registry are Apache-2.0, exist in the builder image
  (the registry as a container the gate starts) and never in the product. cosign and
  syft are pinned by the SHA-256 of the release file, the registry image by digest.

## What the gate verifies, and what a consumer of a key-signed image would

```sh
cosign verify --key cactusfs.pub --insecure-ignore-tlog=true <registry>/cactusfs@sha256:<digest>
cosign verify-attestation --key cactusfs.pub --insecure-ignore-tlog=true --type spdxjson \
  <registry>/cactusfs@sha256:<digest of the platform image>
```

Verify the digest and deploy the digest. A tag is not signed: a signature says "this
digest is a release", so a tag moved to another *released* image still verifies, and
only a digest pins which one runs.

## Releases: keyless (PLAN 7.9; Albie, 2026-10-12)

The owner's decisions: the module path stays `github.com/albievan/cactusfs`; releases
are published to `ghcr.io/albievan/cactusfs`; they are signed **keyless**; the licence
that is declared is the blessing of `LICENSE.md`.

- **Who signs.** Nobody holds a release key: there is none. The workflow
  `.github/workflows/release.yml`, started by a pushed tag `v<version>`, asks GitHub for
  an OIDC token for the job; Fulcio gives a certificate, valid for minutes, for a key
  made for the one signature. The certificate **binds** the signature to the repository
  (`albievan/cactusfs`), the workflow file and the ref it ran at - the identity a
  consumer verifies, exactly:
  `https://github.com/albievan/cactusfs/.github/workflows/release.yml@refs/tags/v<version>`,
  issuer `https://token.actions.githubusercontent.com`. A token of the registry cannot
  produce it; a workflow on a branch, another workflow file or a fork produces another
  identity.
- **What Rekor adds.** Every signature and attestation is recorded in the public
  transparency log (`--tlog-upload` is left on, unlike in the gate). The entry proves
  that the signature was made while the certificate was valid - which is what lets a
  ten-minute certificate be verified years later - and it makes every signature made
  as this identity public: one the project did not make can be found.
- **What is signed, by digest:** the manifest list, the manifest of each platform, the
  chart (an OCI artifact at `ghcr.io/albievan/charts/cactusfs`, packaged with the
  release as its version), and - as a file, into a Sigstore bundle - `SHA256SUMS` of
  the six binaries. The SBOM of each platform image is attached to it as a keyless
  attestation. `scripts/release.sh --publish` verifies each of these against the
  identity right after it made it and stops at the first that does not verify; the tag
  `<version>` is written last, onto a list that is signed and verified. No `latest`.
- **Where the chart goes.** `oci://ghcr.io/albievan/charts`, where helm adds the
  chart's name. Not under `ghcr.io/albievan/cactusfs`: an OCI repository has one set
  of tags, and the chart's `<version>` would be the image's.
- **When a release may be made.** `--publish` refuses unless it runs in GitHub Actions,
  in `albievan/cactusfs`, on a tag, the tag is `v` + `internal/buildinfo/VERSION` (and
  the `Dockerfile` and the chart agree), the workflow is `release.yml` at that tag, the
  tree is a clean commit, an OIDC token can be asked for and the registry's token is
  there. The workflow asks the first seven before anything else (`--preflight`), then
  that the commit is in the history of `main`, then runs `make check` and
  `make test-chaos` in jobs that have no permission to push or to sign.
- **Why the gate keeps a key.** A keyless signature needs the workflow's identity and
  three public services; a gate that must pass without a network can have neither. So
  two modes exist, on purpose, and share everything but the cosign flags
  (`SC_MODE` in `scripts/lib-supplychain.sh`): the gate proves offline, on every
  change, the procedure - push, list, sign by digest, attest, verify, and the five
  refusals - with a key made for the run; a release runs the same functions with a
  certificate instead of the key. What the gate cannot prove is the part only a
  release has: the registry's token exchange against ghcr.io, the keyless command
  lines, `helm push`, and signing a file. Those lines run for the first time in the
  first release; each is verified by the script before it goes on, and the script
  stops if one fails.
- **An air-gapped consumer.** Keyless verification asks Sigstore for its roots of trust
  and reads the log. Three ways, in the order they are recommended: verify where the
  internet is reached and carry the **digest** across (the digest is the trust; deploy
  by digest); mirror the image with its signature and attestation (`cosign copy`) and
  verify inside with Sigstore's trusted root brought in as a file
  (`cosign verify --trusted-root ...`, same identity and issuer), the proof of the log
  entry being stored with the signature; or countersign the digest with a key of the
  site, which verifies as the gate does, with nothing but that key. The second is
  described from cosign's documentation and is not rehearsed by a gate here.
- **The licence that is declared.** `LICENSE.md` dedicates CactusFS to the public
  domain "in the way SQLite is" and gives SQLite's blessing - but in the project's own
  words, with sections SQLite's notice does not have: what is not covered, how
  contributions are dedicated, and a grant of every permission where a dedication is
  not recognised. The SPDX list has `blessing` ("SQLite Blessing"), and its text is
  SQLite's notice, which `LICENSE.md` is not; it is not `Unlicense` or `CC0-1.0` either.
  Declaring any of them would say that another text applies. So the SBOM of an image
  declares, for the package `github.com/albievan/cactusfs`,
  `LicenseRef-CactusFS-Blessing`, and carries the text of `LICENSE.md` under that
  identifier (`hasExtractedLicensingInfos`), as SPDX provides for a licence that is not
  on its list. The chart says the same in `org.opencontainers.image.licenses`, which
  takes an SPDX expression, with a link to the text; it has no
  `artifacthub.io/license`, which takes an identifier of the list. The dependencies
  keep the licences syft finds for them.

## Still the owner's to decide or to do

- Protect the tags `v*` and the branch `main` in the repository's settings (who may
  push them): the workflow checks what a workflow can check, not who pushed.
- The packages `cactusfs` and `charts/cactusfs` on ghcr.io are private when first
  pushed; they must be made public once for anybody to pull and verify.
- Whether a release that was published may be published again under the same version
  (today a second run of the workflow at the same tag would move `<version>` to the new
  list; both would be signed, and both digests stay valid).

## Consequences

- The key of the gate and of the dry run is made for one run and deleted with it. The
  gate proves that the **procedure** works and refuses what it must; it proves nothing
  about where an image came from. Provenance begins with the first release, and is the
  identity of the release workflow, not a key.
- There is no release key to keep, to rotate or to lose. In exchange a release depends
  on GitHub's OIDC, Fulcio and Rekor being reachable when it is made, verification
  depends on Sigstore's roots of trust, and the name of the workflow file and the
  repository are part of what every consumer verifies: renaming either is a change
  consumers must be told of.
- The identity proves which workflow, at which tag, signed. It does not prove that the
  tag was pushed by the owner: that is the repository's protection of tags.
- The builder image gains two tools and the gate one container; `make check` needs the
  engine socket for it, as `make image` already does.
- Moving a pin of cosign means reading its release notes first: the flags for working
  without a transparency log differ between major versions. They are in one place,
  `scripts/lib-supplychain.sh`.
