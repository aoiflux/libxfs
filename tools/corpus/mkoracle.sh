#!/usr/bin/env bash
#
# mkoracle.sh - produce ground truth for each corpus image.
#
# Two independent oracles are produced per image:
#
#   Oracle A (oracle-mount.ndjson)
#       The kernel's own XFS implementation, read through a read-only loop
#       mount. Authoritative, but needs root and XFS support in the kernel.
#
#   Oracle B (ncheck.txt, dirinfo.txt, sb.txt)
#       xfs_db reading the image directly. Needs neither a mount nor kernel
#       XFS support, and is a genuinely separate implementation, so agreement
#       between A and B is real evidence rather than one program agreeing with
#       itself.
#
# Raw tool output is stored verbatim and parsed in Go. Reformatting it here
# would mean quoting arbitrary filenames in shell, and the corpus deliberately
# contains names with spaces and tabs.
#
#	sudo ./mkoracle.sh              # all cases present in the corpus
#	sudo ./mkoracle.sh leaf node    # named cases only

set -euo pipefail

CORPUS_ROOT="${CORPUS_ROOT:-/var/tmp/libxfs-corpus}"
MOUNTPOINT="${MOUNTPOINT:-/mnt/libxfs-corpus}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ORACLE_BIN="${ORACLE_BIN:-/var/tmp/libxfs-oracle}"

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'mkoracle: %s\n' "$*" >&2; exit 1; }

require_root() {
	[ "$(id -u)" -eq 0 ] || die "must run as root (loop mount is required)"
}

build_oracle() {
	command -v go >/dev/null 2>&1 || die "go toolchain not found"
	# GOCACHE is pinned so that running under sudo does not write into the
	# invoking user's cache with root ownership. buildvcs is off because the
	# build runs as root against a repository owned by someone else, and
	# stamping it would mean invoking git purely as a side effect of a build.
	env HOME=/var/tmp GOCACHE=/var/tmp/libxfs-gocache \
		go build -buildvcs=false -o "$ORACLE_BIN" "$HERE/oracle" \
		|| die "failed to build the mount oracle"
	log "built mount oracle at $ORACLE_BIN"
}

# oracle_a mounts read-only and walks with the kernel.
oracle_a() {
	local dir="$1"
	local image="$dir/image.img"

	if mountpoint -q "$MOUNTPOINT" 2>/dev/null; then
		umount "$MOUNTPOINT"
	fi
	mkdir -p "$MOUNTPOINT"

	# norecovery matches how a forensic tool must treat evidence: never write,
	# never replay the log. nouuid allows mounting images that share a UUID.
	if ! mount -o ro,norecovery,nouuid,loop "$image" "$MOUNTPOINT" 2>"$dir/mount.err"; then
		log "$(basename "$dir"): read-only mount failed, Oracle A unavailable"
		cat "$dir/mount.err" >&2
		rm -f "$dir/oracle-mount.ndjson"
		return 1
	fi

	"$ORACLE_BIN" -root "$MOUNTPOINT" > "$dir/oracle-mount.ndjson"
	umount "$MOUNTPOINT"
	log "$(basename "$dir"): Oracle A wrote $(wc -l < "$dir/oracle-mount.ndjson") records"
}

# oracle_b reads the image directly with xfs_db.
oracle_b() {
	local dir="$1"
	local image="$dir/image.img"

	xfs_db -r -c "sb 0" \
		-c "p rootino blocksize dirblklog inodesize versionnum agcount" \
		"$image" > "$dir/sb.txt" 2>&1

	xfs_db -r -c "blockget -n" -c "ncheck" "$image" > "$dir/ncheck.txt" 2>"$dir/ncheck.err"

	# Directory inodes are exactly those ncheck names with a trailing "/.",
	# plus the root, which ncheck never lists because it has no parent entry.
	local rootino
	rootino="$(awk -F'= *' '/rootino/ {print $2}' "$dir/sb.txt" | tr -d ' ')"

	{
		printf '## ino %s path /\n' "$rootino"
		dump_inode "$image" "$rootino"
		awk '
			{
				ino = $1
				line = $0
				sub(/^[ \t]*[0-9]+[ \t]+/, "", line)
				if (line ~ /\/\.$/) {
					sub(/\/\.$/, "", line)
					print ino "\t" line
				}
			}
		' "$dir/ncheck.txt" \
		| while IFS=$'\t' read -r ino relpath; do
			printf '## ino %s path /%s\n' "$ino" "$relpath"
			dump_inode "$image" "$ino"
		done
	} > "$dir/dirinfo.txt"

	log "$(basename "$dir"): Oracle B wrote $(grep -c '^## ino' "$dir/dirinfo.txt") directories, $(wc -l < "$dir/ncheck.txt") named inodes"
}

# dump_inode prints the mapping format and the full logical block map of one
# inode. The block map is what separates a leaf-indexed directory from a
# node-indexed one: index blocks live above the 32 GiB data-space boundary.
dump_inode() {
	local image="$1" ino="$2"
	printf 'inode %s\np core.format core.size core.nextents core.nblocks core.mode core.naextents core.forkoff\nbmap -d\n' "$ino" \
		| xfs_db -r "$image" 2>&1
}

main() {
	require_root
	build_oracle

	local names=("$@")
	if [ ${#names[@]} -eq 0 ]; then
		mapfile -t names < <(find "$CORPUS_ROOT" -mindepth 1 -maxdepth 1 -type d -printf '%f\n' | sort)
	fi

	local name dir
	for name in "${names[@]}"; do
		dir="$CORPUS_ROOT/$name"
		[ -f "$dir/image.img" ] || die "no image for case $name"
		oracle_b "$dir"
		oracle_a "$dir" || true
	done

	printf 'oracles written under %s\n' "$CORPUS_ROOT" >&2
}

main "$@"
