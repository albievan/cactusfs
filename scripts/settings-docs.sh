#!/bin/sh
# Writes the documents that list the settings of "cactusfs server" from what
# the binary itself prints (cmd/cactusfs/settings.go is the one list):
#
#   deploy/cactusfs.env.example   cactusfs config --format env
#   README.md, docs/ARCHITECTURE.md
#                                 cactusfs config --format markdown, between
#                                 the lines <!-- settings:begin --> and
#                                 <!-- settings:end -->
#
# Run through `make settings-docs` (part of `make gen`), or as it is: it needs
# Go and nothing else. TestSettingsDocumentsInSync fails when a committed
# file differs from what this writes.
set -eu

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

go run ./cmd/cactusfs config --format env >"$TMP/env"
go run ./cmd/cactusfs config --format markdown >"$TMP/table"
if [ ! -s "$TMP/env" ] || [ ! -s "$TMP/table" ]; then
	echo "settings-docs: FAIL - cactusfs config printed nothing" >&2
	exit 1
fi

cp "$TMP/env" deploy/cactusfs.env.example
echo "settings-docs: deploy/cactusfs.env.example"

for doc in README.md docs/ARCHITECTURE.md; do
	begins=$(grep -c '^<!-- settings:begin -->$' "$doc" || :)
	ends=$(grep -c '^<!-- settings:end -->$' "$doc" || :)
	if [ "$begins" != 1 ] || [ "$ends" != 1 ]; then
		echo "settings-docs: FAIL - $doc must have one line <!-- settings:begin --> and one <!-- settings:end -->" >&2
		exit 1
	fi
	awk -v table="$TMP/table" '
		$0 == "<!-- settings:begin -->" {
			print
			print ""
			while ((getline line < table) > 0) print line
			close(table)
			print ""
			inside = 1
			next
		}
		$0 == "<!-- settings:end -->" { inside = 0 }
		!inside { print }
	' "$doc" >"$TMP/doc"
	# Written in place, so that the file keeps its mode.
	cat "$TMP/doc" >"$doc"
	echo "settings-docs: $doc"
done
