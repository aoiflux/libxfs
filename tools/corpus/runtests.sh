#!/usr/bin/env bash
#
# runtests.sh - run the corpus-backed tests against the images built by
# mkcorpus.sh and described by mkoracle.sh.
#
#	tools/corpus/runtests.sh                   # the oracle tests
#	tools/corpus/runtests.sh -run TestWalk     # anything `go test` accepts
#
# Runs from the repository root regardless of where it is invoked, and points
# the tests at the corpus through LIBXFS_CORPUS.

set -euo pipefail

CORPUS_ROOT="${CORPUS_ROOT:-/var/tmp/libxfs-corpus}"
REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

[ -d "$CORPUS_ROOT" ] || {
	printf 'runtests: no corpus at %s; run tools/corpus/mkcorpus.sh first\n' "$CORPUS_ROOT" >&2
	exit 1
}

cd "$REPO"

args=("$@")
if [ ${#args[@]} -eq 0 ]; then
	# Every corpus-backed test. They all skip without LIBXFS_CORPUS, so the
	# filter is about keeping the output readable, not about correctness.
	args=(-run 'TestOracle|TestCorpus|TestWalk|TestDirectoryIndexAgrees|TestSourceFormat|TestHashLookup|TestDamaged' -v)
fi

exec env LIBXFS_CORPUS="$CORPUS_ROOT" \
	GOFLAGS=-buildvcs=false \
	go test -count=1 -timeout 30m "${args[@]}" .
