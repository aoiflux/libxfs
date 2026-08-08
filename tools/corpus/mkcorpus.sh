#!/usr/bin/env bash
#
# mkcorpus.sh - build a corpus of real XFS images spanning every directory
# format, so that libxfs's directory walk can be diffed against ground truth.
#
# Synthetic fixtures only prove the parser agrees with the author's reading of
# the format. These images come from mkfs.xfs, which is the only way to catch a
# misunderstanding shared by both the parser and its fixtures.
#
# Must run as root on Linux with xfsprogs and loop-mount support:
#
#	sudo ./mkcorpus.sh              # build every case
#	sudo ./mkcorpus.sh sf node      # build named cases only
#
# Output, one directory per case under $CORPUS_ROOT:
#
#	<case>/image.img      the filesystem
#	<case>/geometry.txt   xfs_info + mkfs version, for provenance
#	<case>/mkfs.args      the exact mkfs.xfs arguments used
#
# Oracles are produced separately by oracle_mount.sh and oracle_xfsdb.sh, so
# that image construction and ground-truth extraction stay independent.

set -euo pipefail

CORPUS_ROOT="${CORPUS_ROOT:-/var/tmp/libxfs-corpus}"
MOUNTPOINT="${MOUNTPOINT:-/mnt/libxfs-corpus}"
IMAGE_SIZE="${IMAGE_SIZE:-320M}"

# Every timestamp in the corpus is pinned so that images are reproducible and
# the oracle's mtimes are stable across rebuilds.
FIXED_TIME="2023-11-14 22:13:20 UTC"

CASE_NAME=""
IMAGE_PATH=""

log() { printf '  %s\n' "$*" >&2; }
die() { printf 'mkcorpus: %s\n' "$*" >&2; exit 1; }

require_root() {
	[ "$(id -u)" -eq 0 ] || die "must run as root (loop mount is required)"
}

require_tools() {
	local missing=()
	for tool in mkfs.xfs xfs_db xfs_info xfs_io setfattr truncate mknod; do
		command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
	done
	[ ${#missing[@]} -eq 0 ] || die "missing tools: ${missing[*]} (apt install xfsprogs attr)"
}

# begin_case creates and mounts a fresh image for a case.
# Usage: begin_case <name> [mkfs args...]
begin_case() {
	CASE_NAME="$1"; shift
	local dir="$CORPUS_ROOT/$CASE_NAME"

	# A previous run that died mid-case leaves the image mounted, and deleting
	# the case directory underneath a live mount would destroy the host's view
	# of it rather than the image's.
	if mountpoint -q "$MOUNTPOINT" 2>/dev/null; then
		umount "$MOUNTPOINT"
	fi

	rm -rf "$dir"
	mkdir -p "$dir"
	IMAGE_PATH="$dir/image.img"

	truncate -s "$IMAGE_SIZE" "$IMAGE_PATH"
	printf '%s\n' "$*" > "$dir/mkfs.args"

	# v4 images make mkfs emit a deprecation notice on stderr; it is not an
	# error and must not abort the build.
	mkfs.xfs -q -f "$@" "$IMAGE_PATH" 2>&1 | grep -v 'deprecated' >&2 || true

	mkdir -p "$MOUNTPOINT"
	mount -o loop "$IMAGE_PATH" "$MOUNTPOINT"
	log "case $CASE_NAME: mounted"
}

# end_case pins timestamps, unmounts, and records geometry.
end_case() {
	local dir="$CORPUS_ROOT/$CASE_NAME"

	# Pin every timestamp, deepest first, so that setting a child's mtime does
	# not perturb the parent directory afterwards.
	find "$MOUNTPOINT" -depth -exec touch -h -d "$FIXED_TIME" {} + 2>/dev/null || true

	sync
	{
		printf '# mkfs.xfs: '; mkfs.xfs -V
		printf '# case: %s\n' "$CASE_NAME"
		xfs_info "$MOUNTPOINT"
	} > "$dir/geometry.txt" 2>&1

	umount "$MOUNTPOINT"

	# A corpus image that does not pass xfs_repair is not ground truth.
	if ! xfs_repair -n "$IMAGE_PATH" >"$dir/repair.txt" 2>&1; then
		die "case $CASE_NAME: xfs_repair -n reported problems (see $dir/repair.txt)"
	fi

	sha256sum "$IMAGE_PATH" | awk '{print $1}' > "$dir/image.sha256"
	log "case $CASE_NAME: done"
}

# ---------------------------------------------------------------------------
# Proto-built cases
#
# mkfs.xfs -p populates a filesystem through userspace libxfs, without ever
# mounting it. That is the only way to build a v4 image on a kernel compiled
# without CONFIG_XFS_SUPPORT_V4, and v4 images are exactly the ones a forensic
# library is most likely to meet in the wild.
#
# Proto format: a boot-file name, a "blocks inodes" line that XFS ignores, then
# the root directory's mode line followed by its entries, each directory
# terminated by "$".
# ---------------------------------------------------------------------------

PROTO_SOURCE_DIR=/var/tmp/libxfs-proto-src

prepare_proto_sources() {
	rm -rf "$PROTO_SOURCE_DIR"
	mkdir -p "$PROTO_SOURCE_DIR"
	: > "$PROTO_SOURCE_DIR/empty"
	repeat_byte 'A' 64 > "$PROTO_SOURCE_DIR/small"
}

# proto_fill_dir emits proto entries for $2 empty files named <prefix>NNN.
proto_fill_dir() {
	local prefix="$1" count="$2"
	seq -w 0 $((count - 1)) | sed "s|^|$prefix|; s|\$| ---644 0 0 $PROTO_SOURCE_DIR/empty|"
}

# begin_proto_case builds and populates an image in one mkfs.xfs invocation.
# The proto description is read from stdin.
begin_proto_case() {
	CASE_NAME="$1"; shift
	local dir="$CORPUS_ROOT/$CASE_NAME"

	rm -rf "$dir"
	mkdir -p "$dir"
	IMAGE_PATH="$dir/image.img"
	prepare_proto_sources

	cat > "$dir/proto.txt"
	truncate -s "$IMAGE_SIZE" "$IMAGE_PATH"
	printf '%s -p proto.txt\n' "$*" > "$dir/mkfs.args"

	mkfs.xfs -q -f "$@" -p "$dir/proto.txt" "$IMAGE_PATH" 2>&1 | grep -v 'deprecated' >&2 || true

	# Geometry comes from xfs_db rather than xfs_info: a v4 image cannot be
	# mounted on this kernel, and xfs_info needs a mount point.
	{
		printf '# mkfs.xfs: '; mkfs.xfs -V
		printf '# case: %s (proto built, never mounted)\n' "$CASE_NAME"
		xfs_db -r -c "sb 0" -c "p" "$IMAGE_PATH"
	} > "$dir/geometry.txt" 2>&1

	if ! xfs_repair -n "$IMAGE_PATH" >"$dir/repair.txt" 2>&1; then
		die "case $CASE_NAME: xfs_repair -n reported problems (see $dir/repair.txt)"
	fi
	sha256sum "$IMAGE_PATH" | awk '{print $1}' > "$dir/image.sha256"
	log "case $CASE_NAME: done (proto)"
}

# fill_dir creates $3 empty entries named <prefix>NNNNNN in directory $1.
# Names are batched through xargs because a shell loop per file is the single
# slowest thing in this script.
fill_dir() {
	local dir="$1" prefix="$2" count="$3" start="${4:-0}"
	mkdir -p "$dir"
	seq -w "$start" $((start + count - 1)) \
		| sed "s|^|$dir/$prefix|" \
		| tr '\n' '\0' \
		| xargs -0 touch
}

# repeat_byte emits $2 copies of character $1.
#
# head must lead the pipeline: if tr is upstream it keeps writing after head has
# taken its quota, takes SIGPIPE, and trips pipefail.
repeat_byte() {
	head -c "$2" /dev/zero | tr '\0' "$1"
}

# write_file writes deterministic content so that per-file digests are stable.
write_file() {
	local path="$1" size="$2"
	mkdir -p "$(dirname "$path")"
	repeat_byte 'A' "$size" > "$path"
}

# ---------------------------------------------------------------------------
# Cases
#
# Each targets one directory format. Thresholds assume the default 4 KiB block
# and directory block size; the extractor records the format actually achieved,
# so a wrong guess here shows up as a failed expectation rather than a silently
# mislabelled fixture.
# ---------------------------------------------------------------------------

# Short-form: entries live inline in the inode's data fork.
case_sf() {
	begin_case sf
	mkdir -p "$MOUNTPOINT/alpha/beta/gamma/delta"
	write_file "$MOUNTPOINT/alpha/one.txt" 100
	write_file "$MOUNTPOINT/alpha/beta/two.txt" 200
	write_file "$MOUNTPOINT/alpha/beta/gamma/three.txt" 300
	write_file "$MOUNTPOINT/alpha/beta/gamma/delta/four.txt" 400
	write_file "$MOUNTPOINT/top.txt" 50
	end_case
}

# Block: too many entries for the inode fork, few enough that data entries, the
# leaf hash array and the tail all fit in a single directory block.
case_block() {
	begin_case block
	fill_dir "$MOUNTPOINT/blockdir" f 40
	write_file "$MOUNTPOINT/blockdir/payload.bin" 1024
	end_case
}

# Leaf: several data blocks plus exactly one leaf index block.
case_leaf() {
	begin_case leaf
	fill_dir "$MOUNTPOINT/leafdir" f 400
	end_case
}

# Node: the leaf index outgrows one directory block, so a da-node is inserted
# above it. A 4 KiB v5 leaf block holds (4096-64)/8 = 504 index entries.
case_node() {
	begin_case node
	fill_dir "$MOUNTPOINT/nodedir" f 3000
	end_case
}

# B+tree-mapped directory: the data fork's extent list overflows the inode
# fork, so the mapping itself becomes a b+tree. Achieved by interleaving growth
# across sibling directories so no directory gets a contiguous run.
case_btree() {
	begin_case btree
	local dirs=8 rounds=24 per=250 d r

	for d in $(seq 0 $((dirs - 1))); do
		mkdir -p "$MOUNTPOINT/frag/d$d"
	done
	for r in $(seq 1 "$rounds"); do
		for d in $(seq 0 $((dirs - 1))); do
			fill_dir "$MOUNTPOINT/frag/d$d" f "$per" $((r * per))
		done
	done
	end_case
}

# Directory block size larger than the filesystem block size.
case_dirblk16k() {
	begin_case dirblk16k -n size=16384
	fill_dir "$MOUNTPOINT/wide" f 3000
	fill_dir "$MOUNTPOINT/narrow" f 20
	end_case
}

# Holes punched into the middle of a directory's data space. Entries occupy
# data blocks in creation order, so deleting a contiguous creation-order range
# empties whole blocks and XFS frees them, leaving a gap below di_size.
case_holes() {
	begin_case holes
	fill_dir "$MOUNTPOINT/holed" f 600
	seq -w 150 400 | sed "s|^|$MOUNTPOINT/holed/f|" | tr '\n' '\0' | xargs -0 rm -f
	end_case
}

# Non-directory inode shapes: sparse, preallocated (unwritten), heavily
# fragmented (b+tree-mapped data fork), hardlinked, symlinked and special.
case_files() {
	begin_case files
	mkdir -p "$MOUNTPOINT/files"

	write_file "$MOUNTPOINT/files/plain.bin" 65536

	# Sparse: a large logical size with only scattered written regions.
	truncate -s $((64 * 1024 * 1024)) "$MOUNTPOINT/files/sparse.bin"
	xfs_io -c "pwrite -S 0x41 0 4096" \
	       -c "pwrite -S 0x42 33554432 4096" \
	       -c "pwrite -S 0x43 67104768 4096" \
	       "$MOUNTPOINT/files/sparse.bin" >/dev/null

	# Unwritten: allocated but never written, so it must read back as zeros
	# while remaining distinguishable from a hole.
	xfs_io -f -c "falloc 0 $((32 * 1024 * 1024))" "$MOUNTPOINT/files/unwritten.bin" >/dev/null

	# Fragmented: punching alternate blocks out of a large file forces the
	# extent list past what the inode fork can hold.
	write_file "$MOUNTPOINT/files/frag.bin" $((16 * 1024 * 1024))
	local off
	for off in $(seq 4096 8192 $((8 * 1024 * 1024))); do
		printf 'fpunch %d 4096\n' "$off"
	done | xfs_io "$MOUNTPOINT/files/frag.bin" >/dev/null

	ln "$MOUNTPOINT/files/plain.bin" "$MOUNTPOINT/files/plain.hardlink"
	ln -s plain.bin "$MOUNTPOINT/files/short.symlink"
	# A symlink target longer than the inode fork forces an extent-form symlink.
	ln -s "$(repeat_byte 'p' 400)" "$MOUNTPOINT/files/long.symlink"

	mkfifo "$MOUNTPOINT/files/pipe"
	mknod "$MOUNTPOINT/files/chardev" c 1 3
	mknod "$MOUNTPOINT/files/blockdev" b 7 0

	setfattr -n user.small -v hello "$MOUNTPOINT/files/plain.bin"
	setfattr -n user.large -v "$(repeat_byte 'x' 800)" "$MOUNTPOINT/files/plain.bin"
	end_case
}

# Name framing edge cases.
case_names() {
	begin_case names
	mkdir -p "$MOUNTPOINT/names"
	write_file "$MOUNTPOINT/names/$(repeat_byte 'n' 255)" 16
	write_file "$MOUNTPOINT/names/with space" 16
	write_file "$MOUNTPOINT/names/with	tab" 16
	write_file "$MOUNTPOINT/names/naïve-café-日本語-🜛" 16
	write_file "$MOUNTPOINT/names/.hidden" 16
	write_file "$MOUNTPOINT/names/trailing." 16
	# A name that is exactly one byte short of an 8-byte framing boundary, on
	# both sides of the boundary, to catch off-by-one padding errors.
	local n
	for n in 1 2 3 4 5 6 7 8 9 15 16 17 23 24 25; do
		write_file "$MOUNTPOINT/names/$(repeat_byte 'z' "$n")" 8
	done
	end_case
}

# v4 (no CRC) headers: smaller data-block and da-block headers throughout.
#
# Built through a proto file because kernels compiled without
# CONFIG_XFS_SUPPORT_V4 refuse to mount v4 at all. Ground truth for these
# images comes from xfs_db alone, which is precisely the case the second oracle
# exists to cover.
case_v4() {
	prepare_proto_sources
	{
		printf 'dummy\n0 0\n'
		printf 'd--755 0 0\n'
		printf 'sfdir d--755 0 0\n'
		printf 'one.txt ---644 0 0 %s/small\n' "$PROTO_SOURCE_DIR"
		printf 'two.txt ---644 0 0 %s/small\n' "$PROTO_SOURCE_DIR"
		printf '$\n'
		printf 'blockdir d--755 0 0\n'
		proto_fill_dir f 40
		printf '$\n'
		printf 'leafdir d--755 0 0\n'
		proto_fill_dir f 400
		printf '$\n'
		printf 'nodedir d--755 0 0\n'
		proto_fill_dir f 3000
		printf '$\n'
		printf 'link.sym l--777 0 0 sfdir/one.txt\n'
		printf '$\n'
	} | begin_proto_case v4 -m crc=0
}

# v4 without the ftype feature: directory entries carry no file-type byte, so
# entry framing is one byte shorter and a walker that classifies entries from
# ftype rather than from the inode will stop recursing here.
case_v4noftype() {
	prepare_proto_sources
	{
		printf 'dummy\n0 0\n'
		printf 'd--755 0 0\n'
		printf 'sfdir d--755 0 0\n'
		printf 'child d--755 0 0\n'
		printf 'deep.txt ---644 0 0 %s/small\n' "$PROTO_SOURCE_DIR"
		printf '$\n'
		printf 'one.txt ---644 0 0 %s/small\n' "$PROTO_SOURCE_DIR"
		printf '$\n'
		printf 'leafdir d--755 0 0\n'
		proto_fill_dir f 400
		printf '$\n'
		printf 'nodedir d--755 0 0\n'
		proto_fill_dir f 3000
		printf '$\n'
		printf '$\n'
	} | begin_proto_case v4noftype -m crc=0 -n ftype=0
}

# 1 KiB filesystem blocks: directory blocks, headers and extent maths all move.
case_geom1k() {
	begin_case geom1k -b size=1024
	fill_dir "$MOUNTPOINT/leafdir" f 400
	fill_dir "$MOUNTPOINT/nodedir" f 3000
	write_file "$MOUNTPOINT/big.bin" $((4 * 1024 * 1024))
	end_case
}

# Entries deleted from a live directory, with the deleted names recorded.
#
# This is the only case that can say anything about what the carver recovers.
# Deletions are contiguous runs in the middle of otherwise busy data blocks,
# because that is the shape that leaves anything to recover: XFS turns a freed
# entry into an unused run whose first four bytes overwrite the start of the
# record, so the first entry of a run loses its inode number, while the entries
# behind it in a coalesced run keep their bytes intact. Deleting scattered
# single entries would destroy each one's header and recover nothing; deleting
# a whole block's worth would free the block and remove it from the directory
# entirely.
case_deleted() {
	begin_case deleted
	local dir="$CORPUS_ROOT/$CASE_NAME"

	fill_dir "$MOUNTPOINT/target" f 600
	mkdir -p "$MOUNTPOINT/keep"
	write_file "$MOUNTPOINT/keep/stays.txt" 32

	# Two runs, in different data blocks, each leaving its block populated.
	{
		seq -w 100 139
		seq -w 300 339
	} | sed 's|^|f|' > "$dir/deleted-names.txt"

	sed "s|^|$MOUNTPOINT/target/|" "$dir/deleted-names.txt" \
		| tr '\n' '\0' | xargs -0 rm -f

	end_case
	log "case $CASE_NAME: deleted $(wc -l < "$dir/deleted-names.txt") entries from /target"
}

# One directory block deliberately destroyed, alongside the intact original.
#
# Every other case asks whether the walk finds everything on a healthy image.
# This one asks what a single unreadable block costs: the whole directory, the
# rest of the directory after it, or just itself. On a forensic tool the answer
# has to be "just itself", and nothing else in the corpus can show that.
case_damaged() {
	begin_case damaged
	# Enough entries for several data blocks, so that block 1 is a middle
	# block. With only two data blocks, damaging block 1 would destroy the last
	# one, and "the walk continued past the damage" would be untestable.
	fill_dir "$MOUNTPOINT/target" f 1200
	mkdir -p "$MOUNTPOINT/intact"
	write_file "$MOUNTPOINT/intact/keep.txt" 32
	end_case

	local dir="$CORPUS_ROOT/$CASE_NAME"
	local clean="$dir/image.img"
	local damaged="$dir/image.damaged.img"
	local inode sector offset

	inode="$(xfs_db -r -c "blockget -n" -c "ncheck" "$clean" 2>/dev/null \
		| awk '$2 == "target/." {print $1}')"
	[ -n "$inode" ] || die "case damaged: could not find the target directory inode"

	# Logical directory block 1, not 0: block 0 holds "." and "..", and losing
	# it would confound "the walk stopped" with "the walk lost its bearings".
	sector="$(printf 'inode %s\ndblock 1\ndaddr\n' "$inode" | xfs_db -r "$clean" 2>&1 \
		| awk '/daddr is/ {print $NF}')"
	[ -n "$sector" ] || die "case damaged: could not locate directory block 1"
	offset=$((sector * 512))

	cp --sparse=always "$clean" "$damaged"
	# A non-zero pattern: an all-zero block is a legitimately punched hole, and
	# skipping it silently is correct behaviour rather than damage.
	repeat_byte 'Z' "$(xfs_db -r -c "sb 0" -c "p blocksize" "$clean" | awk '{print $3}')" \
		| dd of="$damaged" bs=1 seek="$offset" conv=notrunc status=none

	{
		printf 'inode=%s\n' "$inode"
		printf 'logical_directory_block=1\n'
		printf 'byte_offset=%s\n' "$offset"
		printf 'entries_total=1200\n'
	} > "$dir/damaged.txt"

	log "case $CASE_NAME: damaged directory block 1 of inode $inode at byte $offset"
}

# A deep, mixed tree: the closest analogue to a real image, and the case most
# likely to reproduce a recursive-walk undercount.
case_mixed() {
	begin_case mixed
	local deep="$MOUNTPOINT/a/b/c/d/e/f/g/h"
	mkdir -p "$deep"

	# One directory per format, at different depths.
	write_file "$MOUNTPOINT/a/sf.txt" 32
	fill_dir "$MOUNTPOINT/a/b/blockdir" f 40
	fill_dir "$MOUNTPOINT/a/b/c/leafdir" f 400
	fill_dir "$MOUNTPOINT/a/b/c/d/nodedir" f 3000
	fill_dir "$deep/deepleaf" f 400

	# Sibling breadth alongside depth.
	local i
	for i in $(seq -w 1 30); do
		mkdir -p "$MOUNTPOINT/wide/w$i"
		# 10# forces base ten: seq -w zero-pads, and bash reads a leading zero
		# as octal, so "08" would be a syntax error rather than eight.
		write_file "$MOUNTPOINT/wide/w$i/data.bin" $((1024 * 10#$i))
	done

	write_file "$MOUNTPOINT/a/b/c/d/e/payload.bin" $((2 * 1024 * 1024))
	ln -s ../../../../sf.txt "$MOUNTPOINT/a/b/c/d/up.symlink"
	ln "$MOUNTPOINT/a/sf.txt" "$MOUNTPOINT/wide/sf.hardlink"
	setfattr -n user.tag -v mixed "$MOUNTPOINT/a/b/c/leafdir"
	end_case
}

ALL_CASES="sf block leaf node btree dirblk16k holes files names v4 v4noftype geom1k mixed damaged deleted"

main() {
	require_root
	require_tools
	mkdir -p "$CORPUS_ROOT"

	local cases="${*:-$ALL_CASES}"
	local name
	for name in $cases; do
		if ! declare -F "case_$name" >/dev/null; then
			die "unknown case: $name (known: $ALL_CASES)"
		fi
		"case_$name"
	done

	printf 'corpus written to %s\n' "$CORPUS_ROOT" >&2
}

main "$@"
