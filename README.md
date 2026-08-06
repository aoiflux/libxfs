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

- Superblock parsing and validation
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

### Damaged directories

The zero-value options are strict: any framing error aborts the scan. Set
`BestEffort` to keep what was recovered from healthy blocks, resynchronise past
damage, and collect `ReportAnomaly` entries describing what went wrong.
`MaxBlocks` and `MaxEntries` bound the work; when a cap is reached the listing
reports `Truncated` rather than silently looking complete.

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

## Project Status

Beta.

Core parsing features and forensic helper APIs are implemented and tested, with
ongoing hardening around malformed metadata and edge-case directory layouts.
