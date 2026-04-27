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

- Requires Go 1.26+

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
- Directory listing for:
- short-form inline directories
- block-format directories (`XD2*` / `XDD*` / `XDB3`)
- Path resolution and file reads by absolute path
- File extraction support via example tooling
- Fragmentation analysis APIs
- Directory forensics APIs (active entries, deleted slots, carved deleted
  candidates)

Current limitations:

- Full XFS directory format coverage is still incremental; advanced
  multi-block/node cases may need additional hardening
- Deleted entry recovery currently uses heuristic carving and should be treated
  as probabilistic forensic output
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
