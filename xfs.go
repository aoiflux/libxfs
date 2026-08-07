// Package libxfs is a read-only, forensics-oriented parser for the XFS
// filesystem, written in pure Go with no external dependencies.
//
// # Entry points
//
// Open a volume from any io.ReaderAt — a file, a device handle, or a section
// of a larger image — with [Open], or from a path with [OpenVolumeFromPath]:
//
//	volume, err := libxfs.OpenVolumeFromPath("disk.img")
//	if err != nil {
//		return err
//	}
//	defer volume.Close()
//
//	entries, err := volume.ListRootDirectoryEntries()
//	if err != nil {
//		return err
//	}
//	for _, entry := range entries {
//		fmt.Println(entry.Name, entry.InodeNumber, libxfs.DirEntryFileTypeName(entry.FileType))
//	}
//
// From a volume, the API divides into four groups:
//
//   - Geometry and features: [Volume.Superblock], and the feature predicates on
//     [Superblock] such as [Superblock.HasBigTimestamps].
//   - Metadata: [Volume.OpenInode], [Volume.OpenInodeByPath] and the timestamp
//     accessors on [Inode].
//   - Content: [Volume.ReadFileData], [Volume.ReadInodeData] and
//     [Volume.ListInodeExtendedAttributes].
//   - Directories: [Volume.ListDirectoryEntries] for the plain view, and
//     [Volume.ScanDirectoryRecordsWithOptions] for the forensic view that also
//     reports free slots and carved candidates.
//
// # Reading damaged images
//
// The default behaviour is strict: a malformed structure is reported as an
// error rather than guessed at. Forensic callers usually want the opposite, so
// the directory scanners accept [DirectoryScanOptions] with BestEffort set,
// which keeps whatever was recovered from healthy blocks, resynchronises past
// the damage, and records a [ReportAnomaly] for each problem found.
//
// Work is bounded on hostile input. Sizes are validated against the volume's
// own capacity, directory walks allocate a single directory block at a time
// rather than a recorded size, and path resolution is protected against
// directory loops.
//
// # Facts versus candidates
//
// Deleted directory entries are recovered by carving reclaimed space, which is
// inherently probabilistic. Every [DirectoryRecord] therefore says how it was
// obtained: use [DirectoryRecord.IsVerified] for records parsed from intact
// framing and [DirectoryRecord.IsProbabilistic] for carve candidates. Do not
// gate on Confidence alone — an active entry and a strong carve candidate can
// both be [ConfidenceHigh].
//
// # Format coverage
//
// Both v4 and v5 (CRC) filesystems are supported, including short-form, block,
// leaf, node and btree directories, directory blocks larger than the
// filesystem block size, extent-list and b-tree data forks, short-form and
// block-based extended attributes with remote values, 64-bit "bigtime"
// timestamps, and 64-bit (nrext64) extent counters. An image carrying an
// incompatible feature this parser does not understand is refused rather than
// silently misread.
//
// Writing is out of scope; this package never modifies its input.
package libxfs

import (
	"fmt"
	"io"
	"os"
)

// Open parses an XFS volume from a random-access reader.
//
// The reader is not closed by [Volume.Close]; the caller retains ownership.
// Use [OpenVolumeFromPath] when the volume should own its file handle. To read
// a filesystem embedded in a larger image, pass an [io.SectionReader] covering
// the partition.
//
// The returned Volume is safe for concurrent use.
func Open(reader io.ReaderAt) (*Volume, error) {
	if reader == nil {
		return nil, wrapVolumeError("open", fmt.Errorf("reader is nil"))
	}

	v := &Volume{
		reader:     reader,
		inodeCache: make(map[uint64]*Inode),
	}

	if err := v.parseAllocationGroups(); err != nil {
		return nil, wrapVolumeError("open", err)
	}
	return v, nil
}

// OpenVolumeFromPath opens an XFS volume from a filesystem path.
//
// This is a convenience wrapper around os.Open and [Open]. The returned volume
// owns the underlying file handle, and [Volume.Close] will close it.
//
// Raw device access generally requires elevated privileges: run as
// Administrator on Windows and use a path such as \\.\PhysicalDrive0, or read
// a block device such as /dev/sda1 on Linux.
func OpenVolumeFromPath(path string) (*Volume, error) {
	if path == "" {
		return nil, wrapParseError(0, "path", ErrInvalidPath)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, wrapIOError("open", 0, 0, err)
	}

	volume, err := Open(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	volume.sourceCloser = file
	return volume, nil
}
