#!/usr/bin/env bash
#
# mkfixtures.sh - turn corpus images into committed regression fixtures.
#
# The oracle tests need Linux, root, xfsprogs and a freshly built corpus. These
# fixtures carry their conclusions to every other machine: a metadata-only copy
# of each image plus a manifest of what the oracle determined it contains, both
# small enough to commit and usable by a plain `go test` anywhere.
#
#	sudo tools/corpus/mkcorpus.sh
#	sudo tools/corpus/mkoracle.sh
#	tools/corpus/runtests.sh          # the fixtures must be built from images
#	sudo tools/corpus/mkfixtures.sh   # that already passed the oracle tests
#
# xfs_metadump keeps metadata and zeroes file data, so a restored image is
# structurally identical to the original and carries no file contents. That is
# why the fixtures assert structure only; contents are checked against the
# kernel by the oracle tests.

set -euo pipefail

CORPUS_ROOT="${CORPUS_ROOT:-/var/tmp/libxfs-corpus}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
FIXTURE_ROOT="$REPO/testdata/corpus"
WORK="${WORK:-/var/tmp/libxfs-fixture-build}"

# The deliberately corrupted case has no committed fixture: xfs_metadump reads
# an image through the same structures the corruption breaks, so the result
# would not reproduce the damage the case exists to test.
SKIP_CASES="damaged"

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'mkfixtures: %s\n' "$*" >&2; exit 1; }

command -v xfs_metadump >/dev/null 2>&1 || die "xfs_metadump not found (apt install xfsprogs)"
command -v xfs_mdrestore >/dev/null 2>&1 || die "xfs_mdrestore not found"
[ -d "$CORPUS_ROOT" ] || die "no corpus at $CORPUS_ROOT"

rm -rf "$WORK"
mkdir -p "$WORK" "$FIXTURE_ROOT"

names=("$@")
if [ ${#names[@]} -eq 0 ]; then
	mapfile -t names < <(find "$CORPUS_ROOT" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)
fi

built=0
for name in "${names[@]}"; do
	image="$CORPUS_ROOT/$name/image.img"
	[ -f "$image" ] || continue
	case " $SKIP_CASES " in
		*" $name "*)
			log "$name: skipped (no committed fixture for this case)"
			continue
			;;
	esac

	target="$FIXTURE_ROOT/$name"
	mkdir -p "$target"

	# -o keeps names unobfuscated. Without it the corpus's deliberate name
	# edge cases would be replaced by generated ones and the fixture would stop
	# testing what it was built to test.
	#
	# -a copies metadata blocks whole. By default xfs_metadump zeroes the
	# unused parts of them, which is exactly where a deleted directory entry's
	# remains sit: without this the deleted case would restore to an image with
	# nothing left to recover, and its recall test would silently measure the
	# dump tool rather than the carver.
	xfs_metadump -a -o "$image" "$WORK/$name.metadump" 2>/dev/null \
		|| die "$name: xfs_metadump failed"
	xfs_mdrestore "$WORK/$name.metadump" "$WORK/$name.img" 2>/dev/null \
		|| die "$name: xfs_mdrestore failed"

	# The restored image must still be a valid filesystem, or the fixture is
	# testing the restore rather than the parser.
	xfs_repair -n "$WORK/$name.img" >"$WORK/$name.repair" 2>&1 \
		|| die "$name: restored image does not pass xfs_repair (see $WORK/$name.repair)"

	gzip -9 -c "$WORK/$name.img" > "$target/image.img.gz"
	built=$((built + 1))
	log "$name: $(du -h "$target/image.img.gz" | cut -f1) fixture"
done

[ "$built" -gt 0 ] || die "no fixtures built"

# Manifests are written by the test binary, which already knows how to read the
# oracle. Deriving them here would mean a second, divergent implementation.
log "writing manifests from the oracle"
cd "$REPO"
env LIBXFS_CORPUS="$CORPUS_ROOT" LIBXFS_WRITE_FIXTURES=1 GOFLAGS=-buildvcs=false \
	go test -count=1 -run TestWriteCorpusFixtureManifests -v . \
	| sed -n 's/^ *//; /wrote /p' >&2

printf 'built %d fixtures under %s\n' "$built" "$FIXTURE_ROOT" >&2
