# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Fixed

- **v5 directory data header size (behaviour change on real images).** Block
  and data directory blocks on v5 (CRC) filesystems were parsed with a 56 byte
  header instead of 64. `xfs_dir3_data_hdr` is `xfs_dir3_blk_hdr` (48) +
  `best_free[3]` (12) + `pad` (4) = 64; 56 is the size of `xfs_da3_blkinfo`,
  which heads leaf and node blocks rather than data blocks. Every block-format
  directory on a v5 filesystem — the `mkfs.xfs` default since 2014 — was parsed
  eight bytes early, misreading the first entry. Callers who worked around the
  old behaviour will see different output.
- Directory listings no longer read the whole recorded directory size into one
  buffer and parse it as a single block, which produced a misleading
  `ErrInvalidInode` on any directory larger than one block.

### Added

- **Multi-block directory support.** Leaf, node and btree format directories
  are now listed correctly by walking the directory's data-block region one
  directory block at a time. Path resolution inherits this.
- Support for directory blocks larger than the filesystem block size
  (`sb_dirblklog > 0`), up to the 64 KiB format maximum.
- `DirectoryScanOptions` and `DirectoryListing`, with:
  - `BestEffort` — keep records recovered from healthy blocks, resynchronise
    past damage, and report `ReportAnomaly` entries instead of failing.
  - `MaxBlocks` / `MaxEntries` — bound work on hostile images; a capped listing
    reports `Truncated` rather than looking complete.
- `ListDirectoryEntriesWithOptions`, `ScanDirectoryRecordsWithOptions` and
  `ScanDirectoryRecordsByPathWithOptions`.
- `DirectoryParentInode` / `DirectoryParentInodeByPath`, recovering the `..`
  link from short-form headers and from block-backed directories.
- Directory entry file types: `DirectoryEntry.FileType`,
  `DirectoryRecord.FileType`, the `DirEntryFileType*` constants and
  `DirEntryFileTypeName`.
- Record provenance on `DirectoryRecord`:
  - `Kind` (`RecordKindActive`, `RecordKindFreeSlot`, `RecordKindCarved`) —
    confidence alone was never a safe gate, since an active entry and a carve
    candidate could both be `high`.
  - `IsVerified()` and `IsProbabilistic()` helpers.
  - `ConfidenceReasons` evidence codes explaining each confidence value.
  - `BlockIndex` and `LogicalOffset`, making records addressable across a
    multi-block directory. `Offset` remains a within-block value.
- Exported `ConfidenceLow` / `ConfidenceMedium` / `ConfidenceHigh` constants.
- Fuzz targets for the directory, short-form, extent, superblock and inode
  parsers, plus a synthetic image fixture builder covering v4 and v5.
- CI covering Go 1.21 through current stable on Linux, Windows and macOS, with
  `go vet`, `gofmt`, the race detector and a fuzz smoke run.

### Changed

- **Minimum Go version lowered from 1.26.2 to 1.21** to widen the build matrix.
  The patch-pinned floor was not required by any language or library feature in
  use. This is a widening change and breaks no existing consumer.
- Directory entries with a zero name length or inode number are now rejected as
  malformed rather than reported as entries. Inode 0 does not exist in XFS and
  a zero-length name cannot be stored, so either means framing has been lost.
- `DirectoryArtifactReport` honours the report's `VerificationMode`: in
  `best_effort` mode a damaged directory yields partial records plus anomalies
  instead of failing the whole artifact report.

### Security

- Bounded allocation and work on crafted images:
  - Directory walks allocate one directory block at a time rather than the
    recorded `di_size`, which was bounded only by `math.MaxInt`.
  - Directory size is clamped to the volume capacity and to the 32 GiB
    directory data space, with an anomaly recorded.
  - `ReadFileData` rejects sizes larger than the volume can hold.
  - Directory block geometry is validated (power of two, at least the
    filesystem block size, at most 64 KiB) before it is used for framing.
  - Path resolution has a depth cap and a visited-inode set, so a crafted
    directory loop cannot drive unbounded traversal.
  - Best-effort resynchronisation is linear rather than quadratic in the block
    size, and per-block anomaly reporting is capped.

## [0.1.0]

- Initial release.
