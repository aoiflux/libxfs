# libxfs

Thread-safe, forensics-friendly XFS parsing in pure Go.

libxfs reads XFS volumes and disk images with strong validation, typed errors,
and an API designed for tooling, incident response, and data recovery workflows.

## Why libxfs

- Pure Go parser with zero external runtime dependencies
- Concurrency-safe volume APIs for parallel reads
- Corruption-aware parsing with strict bounds checks
- Typed errors with context (`errors.Is` / `errors.As` friendly)
- Practical path-based and inode-based APIs for common workflows
- Built-in support for fragmentation analysis and deleted directory artifact
  scanning

## Installation

```bash
go get github.com/aoiflux/libxfs
```

## Go Version

- Requires Go 1.21+
- Tested on Go 1.21 through current stable, on Linux, Windows and macOS

## Quick Start

```go
package main

import (
	"fmt"
	"log"

	"github.com/aoiflux/libxfs"
)

func main() {
	vol, err := libxfs.OpenVolumeFromPath("disk.img")
	if err != nil {
		log.Fatal(err)
	}
	defer vol.Close()

	sb := vol.Superblock()
	fmt.Printf("XFS v%d block=%d inode=%d root=%d\n",
		sb.FormatVersion,
		sb.BlockSize,
		sb.InodeSize,
		sb.RootDirectoryInodeNumber,
	)

	entries, err := vol.ListRootDirectoryEntries()
	if err != nil {
		log.Fatal(err)
	}
	for _, e := range entries {
		fmt.Printf("%s -> inode %d\n", e.Name, e.InodeNumber)
	}
}
```

## Feature Support

Implemented:

- Superblock parsing and validation, including the v5 feature words
- 64-bit "bigtime" inode timestamps and 64-bit (`nrext64`) extent counters
- AGI and inode-btree driven inode lookup
- Inode parsing (v1/v2/v3 layouts)
- Data reads from inline, extent-list, and extent-btree-backed forks
- Attribute fork parsing and xattr listing
- Short-form and block-based xattrs (including remote values)
- Directory listing for every XFS directory layout:
  - short-form inline directories
  - block-format directories (`XD2B` / `XDB3`)
  - leaf, node and btree directories, walked across all data blocks
  - directory blocks larger than the filesystem block size (`sb_dirblklog > 0`)
- Directory entry file types (ftype) and parent-inode recovery
- Path resolution and file reads by absolute path
- File extraction support via example tooling
- Fragmentation analysis APIs
- Directory forensics APIs (active entries, deleted slots, carved deleted
  candidates)

Current limitations:

- Directory lookups walk data blocks linearly; the leaf/node hash index is not
  used to accelerate path resolution on very large directories
- Deleted entry recovery uses heuristic carving and is explicitly labelled
  probabilistic output (see Directory Forensics below)
- Write support is intentionally out of scope (read-only parsing library)

## API Highlights

Volume-level:

- `Open(reader io.ReaderAt) (*Volume, error)`
- `OpenVolumeFromPath(path string) (*Volume, error)`
- `(*Volume).Close() error`
- `(*Volume).Superblock() Superblock`
- `(*Volume).GetRootInode() (*Inode, error)`

Inode/path access:

- `(*Volume).OpenInode(inodeNumber uint64) (*Inode, error)`
- `(*Volume).OpenInodeByPath(path string) (*Inode, error)`
- `(*Volume).ResolveInodeByPath(path string) (uint64, error)`

Directory and file reads:

- `(*Volume).ListDirectoryEntries(inodeNumber uint64) ([]DirectoryEntry, error)`
- `(*Volume).ListDirectoryEntriesByPath(path string) ([]DirectoryEntry, error)`
- `(*Volume).ListRootDirectoryEntries() ([]DirectoryEntry, error)`
- `(*Volume).ListDirectoryEntriesWithOptions(inodeNumber uint64, options DirectoryScanOptions) (DirectoryListing, error)`
- `(*Volume).DirectoryParentInode(inodeNumber uint64) (uint64, error)`
- `(*Volume).DirectoryParentInodeByPath(path string) (uint64, error)`
- `(*Volume).ReadInodeData(inodeNumber uint64, p []byte, off int64) (int, error)`
- `(*Volume).ReadFileData(inodeNumber uint64) ([]byte, error)`
- `(*Volume).ReadFileDataByPath(path string) ([]byte, error)`

xattrs:

- `(*Volume).ListInodeExtendedAttributes(inodeNumber uint64) ([]ExtendedAttribute, error)`

Forensics:

- `(*Volume).AnalyzeInodeFragmentation(inodeNumber uint64) (FragmentationReport, error)`
- `(*Volume).AnalyzeInodeFragmentationByPath(path string) (FragmentationReport, error)`
- `(*Volume).ScanDirectoryRecords(inodeNumber uint64) ([]DirectoryRecord, error)`
- `(*Volume).ScanDirectoryRecordsByPath(path string) ([]DirectoryRecord, error)`
- `(*Volume).ScanDirectoryRecordsWithOptions(inodeNumber uint64, options DirectoryScanOptions) (DirectoryListing, error)`
- `(*Volume).ScanDirectoryRecordsByPathWithOptions(path string, options DirectoryScanOptions) (DirectoryListing, error)`
- `(*Volume).VerifyDirectoryIndex(inodeNumber uint64) (DirectoryIndexReport, error)`
- `(*Volume).VerifyDirectoryIndexByPath(path string) (DirectoryIndexReport, error)`
- `(*Volume).VolumeIntegrityReport() (VolumeIntegrityReport, error)`
- `(*Volume).InodeForensicReport(inodeNumber uint64) (InodeForensicReport, error)`
- `(*Volume).InodeForensicReportByPath(path string) (InodeForensicReport, error)`
- `(*Volume).DirectoryArtifactReport(inodeNumber uint64) (DirectoryArtifactReport, error)`
- `(*Volume).DirectoryArtifactReportByPath(path string) (DirectoryArtifactReport, error)`
- `(*Volume).Report() (*XFSReport, error)`
- `(*Volume).ReportWithOptions(options ReportOptions) (*XFSReport, error)`

## Directory Forensics

`ScanDirectoryRecords` returns active entries, free-space runs and carved
candidates together. **Confidence alone is not a safe gate**: an intact active
entry and a strong carve candidate can both be `high`. Switch on `Kind`, or use
the helpers.

| Kind | Meaning | Safe to present as fact |
| --- | --- | --- |
| `RecordKindActive` | Parsed from intact directory framing | Yes |
| `RecordKindFreeSlot` | Reclaimed space; carries no name or inode number | Not an entry at all |
| `RecordKindCarved` | Pattern-matched inside reclaimed space | No — candidate only |

```go
listing, err := vol.ScanDirectoryRecordsWithOptions(ino, libxfs.DirectoryScanOptions{
    BestEffort: true, // keep what parsed, report anomalies, instead of failing
})

for _, r := range listing.Records {
    switch {
    case r.IsVerified():
        // Fact: an active entry.
    case r.IsProbabilistic():
        // Candidate: may be stale, partial, or a coincidental byte pattern.
        // r.Confidence and r.ConfidenceReasons explain how strong the match is.
    }
}
```

`ConfidenceReasons` carries machine-readable evidence codes (for example
`tag_matches_offset`, `name_printable`, `aligned_offset`, `ftype_valid`) so
forensic output can show its reasoning rather than assert a verdict.

Every record is addressable within the directory stream via `BlockIndex` and
`LogicalOffset`; `Offset` is only meaningful within its own directory block.

### Directory index verification

Leaf and node format directories carry a hash index alongside their entries.
The kernel maintains both together, so disagreement between them means
something modified the directory without maintaining both.

```go
report, err := vol.VerifyDirectoryIndex(ino)
if err == nil && report.HasIndex && !report.Consistent() {
    // report.MissingFromIndex, report.HashMismatches, report.DanglingIndexEntries
}
```

Directories with no index — short-form and single-block — report `HasIndex`
false and are consistent by definition. Path resolution uses the index when it
is present and falls back to a linear walk whenever it is absent, unreadable,
or inconsistent; an index never reduces what can be recovered.

### Damaged directories

A directory is a sequence of independently framed blocks, so damage to one says
nothing about the others. An unreadable or unrecognisable block costs only
itself: the scan records an anomaly, continues through the remaining blocks, and
returns everything it recovered **alongside** the error. Discarding the rest
would silently remove the whole subtree beneath that directory from a recursive
walk.

`BestEffort` additionally resynchronises *within* a damaged block, recovering
entries either side of the damage instead of stopping at the first bad framing.

Because entries are returned with the error, check both:

```go
listing, err := vol.ListDirectoryEntriesReport(ino)
// listing.Entries is usable even when err != nil.
if errors.Is(err, libxfs.ErrDirectoryTruncated) {
    // A safety cap was reached; the listing is a prefix, not the directory.
}
for _, a := range listing.Anomalies { /* what was skipped, and why */ }
```

`ListDirectoryEntries` returns only the entries, so it cannot distinguish a
complete listing from a partial one — use `ListDirectoryEntriesReport` when
completeness matters. `MaxBlocks` and `MaxEntries` bound the work and set
`Truncated`; when the *default* caps are what stopped the scan the error is
`ErrDirectoryTruncated`, since a caller who did not ask for a cap has no other
way to know one exists.

### Deleted entry recovery

Carved candidates come from entry bytes that survive inside reclaimed space.
What survives is decided by XFS, not by this library: freeing an entry writes a
free-run header over the first four bytes of the record, destroying that entry's
inode number, while entries behind it in a coalesced run keep their bytes
intact.

So recall is a property of the image, not a guarantee. On the regression corpus,
deleting two contiguous runs of 40 entries recovers 78 of the 80 names — each
run loses exactly the record whose header was overwritten. Isolated single
deletions typically recover nothing, and this is expected rather than a defect.

Soundness is the part that is guaranteed and tested: a deleted name is never
reported as a live entry, a live entry is never missing, and a carved record is
never presented as fact.

## Filesystem Features

v5 filesystems record feature flags that change the on-disk layout. libxfs reads
them and adapts; an image carrying an incompatible feature it does not understand
is refused rather than silently misparsed.

```go
sb := vol.Superblock()
sb.HasBigTimestamps()     // 64-bit nanosecond timestamps (mkfs default since 2021)
sb.HasLargeExtentCounts() // nrext64: 64-bit extent counters
sb.NeedsRepair()          // filesystem was left needing repair; treat metadata with suspicion
```

Timestamps matter most here. A bigtime timestamp decoded as the legacy format
yields a date roughly 27 years early, which is plausible enough to pass
unnoticed, so `Inode.HasBigTimestamps` records which encoding was used.

## Error Handling

libxfs uses wrapped typed errors so callers can reliably inspect failures.

```go
if _, err := vol.ResolveInodeByPath("/nope"); err != nil {
	if errors.Is(err, libxfs.ErrInodeNotFound) {
		// path component not found
	}

	var pErr *libxfs.ParseError
	if errors.As(err, &pErr) {
		fmt.Printf("parse field=%s offset=%d\n", pErr.Field, pErr.Offset)
	}
}
```

## Examples

See [examples/README.md](examples/README.md) for full details.

Available examples:

- `examples/basic`: open volume, show metadata, list root directory
- `examples/traverse`: recursive traversal with depth control
- `examples/xattrs`: list inode extended attributes
- `examples/inode_read`: read bytes from inode data stream
- `examples/extract`: extract a file by absolute XFS path
- `examples/fragmentation`: report file fragmentation and logical holes
- `examples/forensics`: inspect active/deleted directory records and carved
  candidates
- `examples/report`: build structured forensic report output (JSON + summary)
- `examples/dirscan`: scan a large or damaged directory with best-effort
  recovery, separating verified entries from carved candidates

Run one example:

```bash
cd examples/basic
go run . <xfs_volume_or_image>
```

## Platform Notes

Raw volume access usually requires elevated privileges.

Windows:

- Run terminal as Administrator
- Use paths like `\\.\\C:` or `\\.\\PhysicalDrive0`

Linux:

- Use block-device paths like `/dev/sda1`
- Prefer read-only / forensic-safe acquisition workflows

Disk image files are supported on all platforms.

## Development

Run checks:

```bash
go test ./...
go vet ./...
```

Conformance tests can be run against a real XFS image. They are skipped unless
an image is supplied:

```bash
LIBXFS_TEST_IMAGE=/path/to/xfs.dd go test ./...
```

Synthetic fixtures only prove the parser agrees with its author's reading of the
format. Testing against an image from `mkfs.xfs` is what catches a
misunderstanding shared by both the parser and its fixtures.

### Walk completeness

A directory walk that returns plausible results is not the same as one that
returns every entry. Establishing completeness needs an oracle that shares no
code with this library, so `tools/corpus/` builds one.

`tools/corpus/mkcorpus.sh` creates a corpus of real images spanning every
directory format — short-form, block, leaf and node, each with both
extent-mapped and b+tree-mapped data forks — plus punched holes, sparse and
preallocated files, hardlinks, device nodes, 255-byte and non-ASCII names,
1 KiB/4 KiB/16 KiB geometries, v4 and v4-without-ftype images, one image with a
directory block deliberately destroyed, and one with a recorded set of entries
deleted from a live directory.

`tools/corpus/mkoracle.sh` describes each image twice: once through a read-only
kernel mount, and once through `xfs_db`, which needs neither a mount nor kernel
XFS support. The two are checked against each other before either is used to
judge libxfs. The second oracle is what covers v4 images, which kernels built
without `CONFIG_XFS_SUPPORT_V4` refuse to mount at all.

Building the corpus needs Linux, root and `xfsprogs`:

```bash
sudo apt install xfsprogs attr
sudo tools/corpus/mkcorpus.sh
sudo tools/corpus/mkoracle.sh
tools/corpus/runtests.sh
```

The tests compare the walk against the oracle path by path and directory by
directory, attribute any shortfall to the on-disk format of the directory that
should have produced it, verify each directory's hash index against its own data
blocks, and check every file's contents against the kernel's digest.

### Committed fixtures

The corpus itself is not committed, but its conclusions are. `testdata/corpus/`
holds a manifest per case recording what the oracle determined that image
contains: total paths, per-directory entry counts, on-disk formats, and digests
over the exact set of paths, inode numbers, kinds and sizes. They are small,
reviewable text, so a change to one is a visible change in what the library is
expected to find.

The images those manifests describe are **not** committed — each is a megabyte
or more of binary. Rebuild them from the corpus with:

```bash
sudo tools/corpus/mkfixtures.sh
```

This writes a metadata-only copy of each image, produced with `xfs_metadump`,
next to its manifest. Once built, `go test ./...` checks them on any platform
with no corpus, no root and no `xfsprogs`. Cases whose image has not been built
are skipped, so the suite passes on a fresh checkout.

The fixtures assert structure — paths, inode numbers, kinds, sizes,
per-directory counts and on-disk formats — and never file contents, because
`xfs_metadump` zeroes file data by design. Contents are checked against the
kernel by the corpus tests.

## Project Status

Core parsing features and forensic helper APIs are implemented and tested,
including validation against real `mkfs.xfs` images, with ongoing hardening
around malformed metadata and edge-case directory layouts.
