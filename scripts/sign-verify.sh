#!/bin/sh
# Signature gate (PLAN 7.3, ADR-0021). Run through `make sign-verify`, after
# `make image` and `make sbom`.
#
# Against a throw-away registry, with a key pair made for this run and deleted
# with it, and without the network:
#
#   - both images are pushed, and each is signed BY DIGEST and verifies;
#   - the SBOM of each is attached to its digest as a signed attestation,
#     which verifies, is about that digest and names the Go module;
#   - and each of these is REFUSED:
#       1. an image that was pushed and never signed;
#       2. a signed tag that was moved to another image;
#       3. a signature, checked with another public key;
#       4. the signature of one image, put on another image;
#       5. the attestation of one image, put on another image;
#     and, besides, an attestation asked of an image that has none and an
#     attestation checked with another public key.
#   - then scripts/release.sh --dry-run: the manifest list and what a release
#     writes, against a registry of its own.
#
# A refusal counts only when cosign says that the signature or attestation
# does not verify. A tool that crashed, did not know a flag or did not reach
# the registry fails the gate (sc_refused, scripts/lib-supplychain.sh). Each
# refusal stands next to a verification that succeeds against the same
# registry with the same flags.
#
# What runs where - the engine pushes to localhost:<port>, cosign speaks to
# the registry over a network of the run - how cosign is called and what is
# cleaned up: the head of scripts/lib-supplychain.sh.
#
# The key proves that the procedure works. It proves nothing about where an
# image came from: there is no release key yet (PLAN 7.9).
set -eu

# shellcheck source=scripts/lib-supplychain.sh
. "$(dirname "$0")/lib-supplychain.sh"

sc_init sign-verify docker cosign python3
sc_need_images
for arch in $SC_ARCHES; do
	sc_sbom_check "$arch" >/dev/null
done

sc_registry_start

repo="$SC_RUN/cactusfs"
tag_a="$VERSION-$BUILD-amd64"
tag_b="$VERSION-$BUILD-arm64"
sc_push "$(sc_local_tag amd64)" "$repo" "$tag_a"
sc_push "$(sc_local_tag arm64)" "$repo" "$tag_b"
digest_a=$(sc_digest "$repo" "$tag_a")
digest_b=$(sc_digest "$repo" "$tag_b")
if [ "$digest_a" = "$digest_b" ]; then
	sc_fail "the images of amd64 and arm64 have one digest ($digest_a)"
fi
ref_a=$(sc_ref "$repo" "$digest_a")
ref_b=$(sc_ref "$repo" "$digest_b")
sc_say "pushed amd64 $digest_a and arm64 $digest_b"

sc_keygen "$SC_WORK/keys"
sc_keygen "$SC_WORK/other"
key="$SC_WORK/keys/cosign.key"
pub="$SC_WORK/keys/cosign.pub"
other="$SC_WORK/other/cosign.pub"
if cmp -s "$pub" "$other"; then
	sc_fail "two key pairs with one public key"
fi

# --- amd64 is signed; arm64 is not, yet -----------------------------------------
sc_must "amd64 is signed by digest" sc_sign "$key" "$ref_a"
sc_must "the signature of amd64 verifies, by digest" sc_verify "$pub" "$ref_a"
sc_must "and by its tag, while the tag names that digest" sc_verify "$pub" "$SC_DIRECT_REG/$repo:$tag_a"

sc_refused_signature "1. an image that was never signed" \
	sc_verify "$pub" "$ref_b"

# The tag of the signed image is given to another image.
sc_push "$(sc_local_tag arm64)" "$repo" "$tag_a"
moved=$(sc_digest "$repo" "$tag_a")
if [ "$moved" != "$digest_b" ]; then
	sc_fail "the tag $tag_a was moved to the other image and names $moved, not $digest_b"
fi
sc_refused_signature "2. a signed tag that was moved to another image" \
	sc_verify "$pub" "$SC_DIRECT_REG/$repo:$tag_a"
sc_must "the digest that was signed verifies as before" sc_verify "$pub" "$ref_a"
sc_push "$(sc_local_tag amd64)" "$repo" "$tag_a"
back=$(sc_digest "$repo" "$tag_a")
if [ "$back" != "$digest_a" ]; then
	sc_fail "the tag $tag_a was put back and names $back, not $digest_a"
fi

sc_refused_signature "3. a signature, checked with another public key" \
	sc_verify "$other" "$ref_a"

# --- the SBOM of amd64 as an attestation ------------------------------------------
sc_must "the SBOM of amd64 is attested to its digest" sc_attest "$key" "$(sc_sbom_path amd64)" "$ref_a"
sc_must "the attestation of amd64 verifies" sc_verify_attestation "$pub" "$ref_a"
checked=$(sc_attestation_check "$digest_a")
sc_say "ok - $checked"
sc_refused_attestation "an attestation asked of an image that has none" \
	sc_verify_attestation "$pub" "$ref_b"
sc_refused_attestation "an attestation, checked with another public key" \
	sc_verify_attestation "$other" "$ref_a"

# --- arm64 gets its own -------------------------------------------------------------
sc_must "arm64 is signed by digest" sc_sign "$key" "$ref_b"
sc_must "the signature of arm64 verifies" sc_verify "$pub" "$ref_b"
sc_must "the SBOM of arm64 is attested to its digest" sc_attest "$key" "$(sc_sbom_path arm64)" "$ref_b"
sc_must "the attestation of arm64 verifies" sc_verify_attestation "$pub" "$ref_b"
checked=$(sc_attestation_check "$digest_b")
sc_say "ok - $checked"

# --- what belongs to one image is put on the other ---------------------------------
# Both are of amd64, genuinely signed, and are now what arm64 has in place of
# its own (sc_transplant, scripts/lib-supplychain.sh).
sc_transplant signature "$repo" "$digest_a" "$digest_b"
sc_refused_signature "4. the signature of one image, put on another image" \
	sc_verify "$pub" "$ref_b"
sc_transplant attestation "$repo" "$digest_a" "$digest_b"
sc_refused_attestation "5. the attestation of one image, put on another image" \
	sc_verify_attestation "$pub" "$ref_b"
sc_must "amd64 verifies to the end" sc_verify "$pub" "$ref_a"

# --- what a release would do ---------------------------------------------------------
"$(dirname "$0")/release.sh" --dry-run

echo "sign-verify: PASS"
