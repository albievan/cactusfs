#!/bin/sh
# Says which build this tree would give - one word, for the Makefile and for
# scripts/image.sh:
#
#   scripts/buildid.sh version   the release: what internal/buildinfo/VERSION names
#   scripts/buildid.sh build     the number of the build: the count of commits of HEAD
#   scripts/buildid.sh commit    the commit, 12 characters, with -dirty when files
#                                that git tracks have uncommitted changes
#
# It never fails a build: without git, or outside a repository (an export, a
# container build), the build is 0 and the commit is empty, and the binary
# says "local build". Nothing is written, the index of git included.
set -u

root=$(dirname "$0")/..

case ${1:-} in
version)
	# The release goes into command lines, a tag and a label: only what a
	# semantic version is written with is let through, and nothing else is
	# said than nothing.
	release=
	if [ -r "$root/internal/buildinfo/VERSION" ]; then
		release=$(tr -d ' \t\r\n' <"$root/internal/buildinfo/VERSION")
	fi
	case $release in
	'' | *[!0-9A-Za-z.-]*) echo ;;
	*) echo "$release" ;;
	esac
	;;
build)
	count=$(git -C "$root" rev-list --count HEAD 2>/dev/null) || count=
	case $count in
	'' | *[!0-9]*) count=0 ;;
	esac
	echo "$count"
	;;
commit)
	commit=$(git -C "$root" rev-parse --verify --short=12 'HEAD^{commit}' 2>/dev/null) || commit=
	case $commit in
	'' | *[!0-9a-f]*)
		echo
		exit 0
		;;
	esac
	if [ -n "$(git -C "$root" --no-optional-locks status --porcelain --untracked-files=no 2>/dev/null)" ]; then
		commit=$commit-dirty
	fi
	echo "$commit"
	;;
*)
	echo "usage: $0 version|build|commit" >&2
	exit 2
	;;
esac
