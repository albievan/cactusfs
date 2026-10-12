#!/bin/sh
# SBOM gate (PLAN 7.3, ADR-0021). Writes, as SPDX 2.3 JSON, to dist/sbom/:
#
#   cactusfs-<version>-<build>-linux-amd64.spdx.json   what is in the image
#   cactusfs-<version>-<build>-linux-arm64.spdx.json   of each architecture
#   cactusfs-console-<version>-<build>.spdx.json       web/pnpm-lock.yaml
#
# and fails unless each is an SPDX document that names what it must: the Go
# module itself and a module it is known to link for an image, two packages
# the console is known to use for the lock file. Run through `make sbom`,
# after `make image`: the images are read from the engine's store.
#
# syft makes them, from files and without the network. How it is called, and
# what is asserted, is in scripts/lib-supplychain.sh.
set -eu

# shellcheck source=scripts/lib-supplychain.sh
. "$(dirname "$0")/lib-supplychain.sh"

sc_init sbom docker syft python3
sc_need_images

for arch in $SC_ARCHES; do
	sc_say "the image $(sc_local_tag "$arch")"
	sc_sbom_image "$arch"
done
sc_say "the console's packages (web/pnpm-lock.yaml)"
sc_sbom_console

sc_say "wrote $(sc_sbom_path amd64), $(sc_sbom_path arm64) and $(sc_sbom_path console)"
echo "sbom: PASS"
