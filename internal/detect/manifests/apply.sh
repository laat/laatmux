#!/bin/sh
# The manifests the engine embeds, claude.toml and codex.toml here, are
# herdr's copies in upstream/ with laatmux's own changes on top, kept
# as patches/<name>.patch so a change upstream is taken as it is and
# the local one is reapplied, or fails to and is rebased by hand.
#
#   apply.sh           rebuild every manifest from upstream/ and patches/
#   apply.sh refresh   remake patches/ from the manifests as edited here
#
# TestManifestsPatched in ../detect_test.go fails until the manifests
# and the patches agree.
set -eu
cd "$(dirname "$0")"
case "${1:-apply}" in
apply)
	for f in upstream/*.toml; do
		n=${f#upstream/}
		cp "$f" "$n"
		if [ -f "patches/$n.patch" ]; then
			# -F0: exact context only, so a change upstream near a hunk
			# is a failure to look at, not a fuzzy match.
			patch -p1 -F0 -s --no-backup-if-mismatch "$n" <"patches/$n.patch"
		fi
	done
	;;
refresh)
	for f in upstream/*.toml; do
		n=${f#upstream/}
		if diff -u --label "a/$n" --label "b/$n" "$f" "$n" >"patches/$n.patch"; then
			rm "patches/$n.patch"
		fi
	done
	;;
*)
	echo "usage: apply.sh [apply|refresh]" >&2
	exit 2
	;;
esac
