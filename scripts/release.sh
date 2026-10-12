#!/bin/sh
# What a release does with the images, the chart and the binaries.
#
#   scripts/release.sh --dry-run     the rehearsal: a registry and a key of the run
#   scripts/release.sh --preflight   is this tree, at this tag, a release? Nothing else
#   scripts/release.sh --publish     the release: the release workflow, and nobody else
#
# All three do the same things with the same functions (scripts/lib-supplychain.sh;
# PLAN 7.3 and 7.9, ADR-0021), after `make image` and `make sbom`:
#
#   - push the image of each architecture;
#   - make the multi-arch manifest list <version>-<build> of the two;
#   - sign, BY DIGEST, the list and the image of each platform;
#   - attach the SBOM of each platform image to it as a signed attestation;
#   - verify all of it;
#   - write dist/release/: SHA256SUMS of the binaries of `make build-all`, the
#     SBOMs, the digests;
#   - print the commands somebody would verify the release with.
#
# --dry-run (`make sign-verify` runs it): against a throw-away registry, with a
# key pair made for the run, no transparency log and no network. It pushes to
# no registry but the one it starts. dist/release/ also gets the public key of
# the rehearsal, and SHA256SUMS only when dist/ has binaries.
#
# --publish (`make release`, in .github/workflows/release.yml): to
# $RELEASE_REGISTRY/$RELEASE_REPOSITORY, signed KEYLESS - a certificate of
# Fulcio for the identity of the release workflow at the tag, an entry in
# Rekor. Besides the above it gives the list the tag <version> once it is
# signed and verified, packages the chart as the release, pushes it to
# oci://$RELEASE_REGISTRY/$RELEASE_CHARTS and signs its digest, signs
# SHA256SUMS into SHA256SUMS.sigstore.json, and writes NOTES.md for the
# GitHub Release (which the workflow creates: `gh` is on the runner, not in
# the builder). It never writes the tag `latest`.
#
# --publish REFUSES unless every one of these holds, and --preflight asks the
# same of all but the last two (it runs before the gate, in a job that can
# neither sign nor push):
#
#   exit 10  GITHUB_ACTIONS is true
#   exit 11  GITHUB_REPOSITORY is $RELEASE_REPOSITORY
#   exit 12  GITHUB_REF_TYPE is tag
#   exit 13  internal/buildinfo/VERSION is a semantic version (and VERSION, if given, is it)
#   exit 14  the tag is v<that version>
#   exit 15  the Dockerfile's ARG VERSION and the chart's appVersion are that version
#   exit 16  the workflow is $RELEASE_WORKFLOW at that tag: the identity that will sign
#   exit 17  the tree is a commit without uncommitted changes (and COMMIT, if given, is it)
#   exit 18  the job can ask for an OIDC token (permissions: id-token: write)
#   exit 19  there is a user and a token for the registry
#
# With no argument it refuses (exit 1); with anything else it says how it is
# used (exit 2). There is no way to publish from a branch or from a desk.
#
# The manifest list is written with the registry's HTTP API by the helper of
# scripts/lib-supplychain.sh: two descriptors in a JSON document. The same on
# every engine and image store; an image that was built with attestations is
# itself an index, and its platform manifest is what goes into the list.
set -eu

# Where a release goes: these three lines and nowhere else. The repository of
# the images is also the GitHub repository that may release.
RELEASE_REGISTRY=ghcr.io
RELEASE_REPOSITORY=albievan/cactusfs
# helm adds the name of the chart: the chart is $RELEASE_CHARTS/cactusfs. Not
# under $RELEASE_REPOSITORY: there its tag <version> would be the image's.
RELEASE_CHARTS=albievan/charts
# Who signs: this workflow of that repository, at the tag of the release.
RELEASE_SERVER=https://github.com
RELEASE_WORKFLOW=.github/workflows/release.yml
RELEASE_CHART=deploy/helm/cactusfs
RELEASE_BINARIES="cactusfs-linux-amd64 cactusfs-linux-arm64 cactusfs-darwin-amd64 cactusfs-darwin-arm64 cactusfs-windows-amd64.exe cactusfs-windows-arm64.exe"

usage() {
	echo "usage: $0 --dry-run | --preflight | --publish" >&2
	exit 2
}

case ${1:-} in
--dry-run | --preflight | --publish)
	if [ "$#" -ne 1 ]; then
		usage
	fi
	mode=${1#--}
	;;
'')
	echo "release: REFUSED - say which. $0 --dry-run rehearses a release against a registry" >&2
	echo "         of its own; --publish is for the release workflow, on a tag v<version>, alone." >&2
	exit 1
	;;
*)
	usage
	;;
esac

here=$(dirname "$0")

# refuse <exit status> <why>: a release that must not be made is not made.
refuse() {
	echo "release: REFUSED - $2" >&2
	exit "$1"
}

# preflight publish|preflight: see the head of this file. It reads the
# environment and the tree; it needs no container engine and calls nothing.
preflight() {
	[ "${GITHUB_ACTIONS:-}" = true ] ||
		refuse 10 "not in GitHub Actions (GITHUB_ACTIONS is not 'true'): a release is made by the release workflow"
	[ "${GITHUB_REPOSITORY:-}" = "$RELEASE_REPOSITORY" ] ||
		refuse 11 "the repository is '${GITHUB_REPOSITORY:-}', and releases are made by $RELEASE_REPOSITORY"
	[ "${GITHUB_REF_TYPE:-}" = tag ] ||
		refuse 12 "the ref is a '${GITHUB_REF_TYPE:-}', not a tag: a release is made from a tag v<version>"

	release_version=$(sh "$here/buildid.sh" version)
	echo "$release_version" | grep -Eq '^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$' ||
		refuse 13 "internal/buildinfo/VERSION names '$release_version', which is not a semantic version"
	[ "${VERSION:-$release_version}" = "$release_version" ] ||
		refuse 13 "VERSION=$VERSION was given and internal/buildinfo/VERSION names $release_version"
	[ "${GITHUB_REF_NAME:-}" = "v$release_version" ] ||
		refuse 14 "the tag is '${GITHUB_REF_NAME:-}' and the tree is release $release_version: the tag must be v$release_version"

	in_dockerfile=$(sed -n 's/^ARG VERSION=//p' "$here/../Dockerfile")
	in_chart=$(sed -n 's/^appVersion: "\([^"]*\)"$/\1/p' "$here/../$RELEASE_CHART/Chart.yaml")
	[ "$in_dockerfile" = "$release_version" ] ||
		refuse 15 "the Dockerfile has ARG VERSION=$in_dockerfile and the release is $release_version"
	[ "$in_chart" = "$release_version" ] ||
		refuse 15 "$RELEASE_CHART/Chart.yaml has appVersion '$in_chart' and the release is $release_version"

	[ "${GITHUB_WORKFLOW_REF:-}" = "$RELEASE_REPOSITORY/$RELEASE_WORKFLOW@refs/tags/v$release_version" ] ||
		refuse 16 "the workflow is '${GITHUB_WORKFLOW_REF:-}', and a release is signed as $RELEASE_REPOSITORY/$RELEASE_WORKFLOW@refs/tags/v$release_version"

	release_commit=$(sh "$here/buildid.sh" commit)
	case $release_commit in
	'') refuse 17 "no commit: this is not a git checkout" ;;
	*-dirty) refuse 17 "the tree has uncommitted changes ($release_commit): a release is of a commit" ;;
	esac
	[ "${COMMIT-$release_commit}" = "$release_commit" ] ||
		refuse 17 "COMMIT=$COMMIT was given and the tree is $release_commit"

	if [ "$1" = publish ]; then
		[ -n "${ACTIONS_ID_TOKEN_REQUEST_URL:-}" ] && [ -n "${ACTIONS_ID_TOKEN_REQUEST_TOKEN:-}" ] ||
			refuse 18 "no OIDC token to be had (ACTIONS_ID_TOKEN_REQUEST_URL and _TOKEN): the job needs 'id-token: write'"
		[ -n "${RELEASE_REGISTRY_USER:-}" ] && [ -n "${RELEASE_REGISTRY_TOKEN:-}" ] ||
			refuse 19 "no RELEASE_REGISTRY_USER or no RELEASE_REGISTRY_TOKEN for $RELEASE_REGISTRY"
	fi

	VERSION=$release_version
	COMMIT=$release_commit
	export VERSION COMMIT
}

case $mode in
preflight)
	preflight preflight
	echo "release (preflight): tag v$VERSION, commit $COMMIT, $RELEASE_REPOSITORY/$RELEASE_WORKFLOW: this can be released"
	exit 0
	;;
publish)
	preflight publish
	;;
esac

# shellcheck source=scripts/lib-supplychain.sh
. "$(dirname "$0")/lib-supplychain.sh"

if [ "$mode" = publish ]; then
	sc_init release docker cosign python3 sha256sum helm
else
	sc_init "release (dry run)" docker cosign python3 sha256sum
fi
sc_need_images
for arch in $SC_ARCHES console; do
	sc_sbom_check "$arch" >/dev/null
done

if [ "$mode" = publish ]; then
	# Everything a release hands out is there, and of this commit, before
	# anything is pushed.
	sc_need_revision "$COMMIT"
	for binary in $RELEASE_BINARIES; do
		[ -s "dist/$binary" ] || sc_fail "no dist/$binary; run 'make build-all' first"
	done
	# What Fulcio writes into the certificate for this job, and what preflight
	# has held GITHUB_WORKFLOW_REF to:
	# https://github.com/<repository>/<workflow file>@refs/tags/v<version>
	identity="$RELEASE_SERVER/$GITHUB_WORKFLOW_REF"
	SC_REGISTRY_PASSWORD=$RELEASE_REGISTRY_TOKEN
	unset RELEASE_REGISTRY_TOKEN
	sc_registry_remote "$RELEASE_REGISTRY" "$RELEASE_REGISTRY_USER"
	sc_keyless "$identity"
	sc_say "signing as $identity"
	repo=$RELEASE_REPOSITORY
	name="$RELEASE_REGISTRY/$RELEASE_REPOSITORY"
else
	sc_registry_start
	repo="$SC_RUN/cactusfs"
	name=cactusfs
fi

release="$VERSION-$BUILD"
sources=
for arch in $SC_ARCHES; do
	sc_push "$(sc_local_tag "$arch")" "$repo" "$release-$arch"
	sources="$sources $release-$arch=$arch"
done

# The image of each platform, and the list of them.
for arch in $SC_ARCHES; do
	sc_helper platform "$repo" "$release-$arch" "$arch" >"$SC_WORK/platform-$arch" ||
		sc_fail "no linux/$arch image manifest behind $repo:$release-$arch"
done
# shellcheck disable=SC2086 # the sources are split on purpose
list=$(sc_helper index "$repo" "$release" $sources)
stored=$(sc_digest "$repo" "$release")
if [ "$stored" != "$list" ]; then
	sc_fail "the manifest list was written as $list and the registry has $stored"
fi
sc_say "manifest list $release is $list"

# Who signs and who is believed. In a release there is no key: sc_keyless said
# who, and "-" stands where the key of the run is passed.
if [ "$mode" = publish ]; then
	key=-
	pub=-
else
	sc_keygen "$SC_WORK/keys"
	key="$SC_WORK/keys/cosign.key"
	pub="$SC_WORK/keys/cosign.pub"
fi

sc_must "the manifest list is signed by digest" sc_sign "$key" "$(sc_ref "$repo" "$list")"
sc_must "the signature of the manifest list verifies" sc_verify "$pub" "$(sc_ref "$repo" "$list")"
for arch in $SC_ARCHES; do
	digest=$(cat "$SC_WORK/platform-$arch")
	ref=$(sc_ref "$repo" "$digest")
	sc_must "linux/$arch $digest is signed by digest" sc_sign "$key" "$ref"
	sc_must "the signature of linux/$arch verifies" sc_verify "$pub" "$ref"
	sc_must "the SBOM of linux/$arch is attested to it" sc_attest "$key" "$(sc_sbom_path "$arch")" "$ref"
	sc_must "the attestation of linux/$arch verifies" sc_verify_attestation "$pub" "$ref"
	checked=$(sc_attestation_check "$digest")
	sc_say "ok - $checked"
done

# What a release hands out besides the images. The directory is this
# script's: it is written anew.
out=dist/release
rm -rf "$out"
mkdir -p "$out/sbom"
for arch in $SC_ARCHES console; do
	cp "$(sc_sbom_path "$arch")" "$out/sbom/"
done
{
	echo "$name:$release $list"
	for arch in $SC_ARCHES; do
		echo "$name:$release linux/$arch $(cat "$SC_WORK/platform-$arch")"
	done
} >"$out/digests.txt"
if [ "$mode" != publish ]; then
	# The PUBLIC key, and named for what it is: the key of a rehearsal.
	cp "$pub" "$out/cosign-dry-run.pub"
fi
binaries=
for binary in dist/cactusfs-*; do
	if [ -f "$binary" ]; then
		binaries="$binaries ${binary#dist/}"
	fi
done
if [ -n "$binaries" ]; then
	# shellcheck disable=SC2086 # the names are split on purpose
	(cd dist && sha256sum $binaries) >"$out/SHA256SUMS"
	(cd dist && sha256sum --check --quiet "../$out/SHA256SUMS") || sc_fail "$out/SHA256SUMS does not check"
	sc_say "$out/SHA256SUMS: $(wc -l <"$out/SHA256SUMS" | tr -d ' ') binaries"
else
	sc_say "no binaries in dist/ ('make build-all' makes them): no SHA256SUMS in this rehearsal"
fi

if [ "$mode" != publish ]; then
	cat <<EOF
release (dry run): wrote $out/ - digests.txt, sbom/, cosign-dry-run.pub$([ -n "$binaries" ] && echo ", SHA256SUMS")
release (dry run): the registry and the key of this run are gone when it ends: nothing
release (dry run): was published. A release would be verified with (ADR-0021):

  cosign verify --key <the release's public key> $SC_COSIGN_VERIFY_FLAGS \\
    <registry>/cactusfs@$list
  cosign verify-attestation --key <the release's public key> $SC_COSIGN_VERIFY_FLAGS --type $SC_ATTESTATION_TYPE \\
    <registry>/cactusfs@<digest of the platform image, from digests.txt>
  sha256sum --check SHA256SUMS

EOF
	echo "release (dry run): PASS"
	exit 0
fi

# --- a release only, from here on ----------------------------------------------------

# The binaries: the six, no more and no fewer, and their checksums signed.
count=$(wc -l <"$out/SHA256SUMS" | tr -d ' ')
# shellcheck disable=SC2086 # the names are counted
set -- $RELEASE_BINARIES
if [ "$count" -ne "$#" ]; then
	sc_fail "dist/ holds $count files named cactusfs-*, and a release has $#: $RELEASE_BINARIES"
fi
sc_must "SHA256SUMS is signed" sc_sign_blob "$out/SHA256SUMS" "$out/SHA256SUMS.sigstore.json"
sc_must "the signature of SHA256SUMS verifies" sc_verify_blob "$out/SHA256SUMS" "$out/SHA256SUMS.sigstore.json"

# The chart, as the release: its version and its appVersion are the release's.
chart_repo=$(sc_chart_push "$RELEASE_CHART" "$VERSION" "$RELEASE_CHARTS" "$SC_WORK/chart")
chart=$(sc_digest "$chart_repo" "$VERSION")
sc_say "chart $chart_repo:$VERSION is $chart"
sc_must "the chart is signed by digest" sc_sign "$key" "$(sc_ref "$chart_repo" "$chart")"
sc_must "the signature of the chart verifies" sc_verify "$pub" "$(sc_ref "$chart_repo" "$chart")"
echo "$RELEASE_REGISTRY/$chart_repo:$VERSION $chart" >>"$out/digests.txt"

# The tag people will read, last: it never names a list that is not signed
# and verified. There is no tag `latest`.
tagged=$(sc_helper retag "$repo" "$release" "$VERSION") || sc_fail "the list did not get the tag $VERSION"
if [ "$tagged" != "$list" ]; then
	sc_fail "$repo:$VERSION was written as $tagged, and the list is $list"
fi
echo "$name:$VERSION $list" >>"$out/digests.txt"
sc_say "$name:$VERSION is the list $list"

cat >"$out/NOTES.md" <<EOF
CactusFS $VERSION, build $BUILD, commit \`$COMMIT\`.

| What | Where | Digest |
|---|---|---|
| image, linux/amd64 and linux/arm64 | \`$name:$VERSION\`, \`$name:$release\` | \`$list\` |
| image, linux/amd64 | \`$name:$release-amd64\` | \`$(cat "$SC_WORK/platform-amd64")\` |
| image, linux/arm64 | \`$name:$release-arm64\` | \`$(cat "$SC_WORK/platform-arm64")\` |
| Helm chart | \`oci://$RELEASE_REGISTRY/$chart_repo\`, version \`$VERSION\` | \`$chart\` |

Everything is signed keyless (Sigstore) by the release workflow at this tag. Verify a
**digest** and deploy that digest:

\`\`\`sh
IDENTITY=$identity
ISSUER=$SC_KEYLESS_ISSUER

cosign verify --certificate-identity "\$IDENTITY" --certificate-oidc-issuer "\$ISSUER" \\
  $name@$list

# the SBOM (SPDX) of a platform image, attested to its digest
cosign verify-attestation --type $SC_ATTESTATION_TYPE \\
  --certificate-identity "\$IDENTITY" --certificate-oidc-issuer "\$ISSUER" \\
  $name@$(cat "$SC_WORK/platform-amd64")

# the chart
cosign verify --certificate-identity "\$IDENTITY" --certificate-oidc-issuer "\$ISSUER" \\
  $RELEASE_REGISTRY/$chart_repo@$chart

# the binaries
cosign verify-blob $SC_KEYLESS_BUNDLE_FLAGS --bundle SHA256SUMS.sigstore.json \\
  --certificate-identity "\$IDENTITY" --certificate-oidc-issuer "\$ISSUER" SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS
\`\`\`

\`digests.txt\` repeats the digests above; it is a convenience and is not signed - what
is signed is each digest itself. The \`*.spdx.json\` files are the SBOMs of the two
images and of the console's packages.
EOF

cat <<EOF
release: published $name:$VERSION and :$release ($list),
release: the chart $RELEASE_REGISTRY/$chart_repo:$VERSION ($chart), signed as
release:   $identity
release: wrote $out/ - digests.txt, sbom/, SHA256SUMS, SHA256SUMS.sigstore.json, NOTES.md
EOF
echo "release: PASS"
