#!/bin/sh
# Writes the secrets of the development cluster of deploy/compose/compose.yaml
# into the files compose hands to the nodes as secrets (/run/secrets/<name>),
# so that no secret is in the environment of a container, where `inspect`
# shows it to whoever may use the engine. Run by `make up`; run it yourself
# before `docker compose up` or `podman compose up` without make.
#
#   CACTUSFS_KMS_PASSPHRASE   the passphrase of the cluster's KMS key file:
#                             written when exported; required the first time,
#                             at least 12 characters, the same at every start
#   CACTUSFS_ROOT_ACCESS_KEY  the root credentials of a new deployment, both
#   CACTUSFS_ROOT_SECRET_KEY  or neither: written when exported. With neither
#                             and no files yet, the files are written empty,
#                             which a node takes as none given: one node of a
#                             new deployment then generates a pair and prints
#                             it once, into its container log.
#
# The files are in deploy/compose/secrets/, where compose.yaml names them; git
# ignores the directory and it never enters the build context. The directory is the user's alone (0700). The files in it are readable by
# every user (0444): the nodes run as an unprivileged user of their own
# (65532), which reads them through the mount and never through the
# directory. They hold secrets in plain text on this machine: this is a
# development cluster. Production keeps them in Kubernetes Secrets or Vault.
set -eu

dir=$(dirname "$0")/../deploy/compose/secrets
umask 077
mkdir -p "$dir"
chmod 0700 "$dir"

# put <name> <value>: the file holds the value and nothing else.
put() {
	rm -f "$dir/$1"
	printf '%s' "$2" >"$dir/$1"
	chmod 0444 "$dir/$1"
}

passphrase=${CACTUSFS_KMS_PASSPHRASE:-}
if [ -n "$passphrase" ]; then
	if [ "${#passphrase}" -lt 12 ]; then
		echo "compose-secrets: FAIL - CACTUSFS_KMS_PASSPHRASE has fewer than 12 characters" >&2
		exit 1
	fi
	put kms-passphrase "$passphrase"
elif [ ! -s "$dir/kms-passphrase" ]; then
	echo "compose-secrets: FAIL - set CACTUSFS_KMS_PASSPHRASE to a passphrase of at least 12 characters" >&2
	exit 1
fi

id=${CACTUSFS_ROOT_ACCESS_KEY:-}
secret=${CACTUSFS_ROOT_SECRET_KEY:-}
if [ -n "$id" ] && [ -n "$secret" ]; then
	put root-access-key "$id"
	put root-secret-key "$secret"
elif [ -n "$id" ] || [ -n "$secret" ]; then
	echo "compose-secrets: FAIL - set both CACTUSFS_ROOT_ACCESS_KEY and CACTUSFS_ROOT_SECRET_KEY, or neither" >&2
	exit 1
elif [ ! -f "$dir/root-access-key" ] || [ ! -f "$dir/root-secret-key" ]; then
	# Compose needs a file for every secret; empty ones name no credentials.
	put root-access-key ""
	put root-secret-key ""
fi
echo "compose-secrets: $dir"
