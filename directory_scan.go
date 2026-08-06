package libxfs

import (
	"errors"
	"fmt"
	"io"
)

// Default safety caps applied when DirectoryScanOptions leaves them unset.
//
// These bound work and allocation on damaged or hostile images. A directory
// large enough to trip them is already far outside anything a real filesystem
// produces.
const (
	defaultMaxDirectoryBlocks  = uint64(1) << 20
	defaultMaxDirectoryEntries = 1 << 22
)

// DirectoryScanOptions controls how a directory is walked.
//
// The zero value is strict: any framing error aborts the scan, and only
// active entries are reported.
type DirectoryScanOptions struct {
	// IncludeDeleted reports free-space runs and carved candidates alongside
	// active entries.
	IncludeDeleted bool
	// BestEffort keeps whatever was recovered when a block is malformed,
	// recording a ReportAnomaly and resynchronising instead of failing. This
	// is usually what forensic callers want on a damaged image.
	BestEffort bool
	// MaxBlocks caps the number of directory blocks read. Zero applies the
	// default cap.
	MaxBlocks uint64
	// MaxEntries caps the number of records collected. Zero applies the
	// default cap.
	MaxEntries int
}

func (o DirectoryScanOptions) maxBlocks() uint64 {
	if o.MaxBlocks == 0 {
		return defaultMaxDirectoryBlocks
	}
	return o.MaxBlocks
}

func (o DirectoryScanOptions) maxEntries() int {
	if o.MaxEntries == 0 {
		return defaultMaxDirectoryEntries
	}
	return o.MaxEntries
}

// DirectoryListing is the result of a directory scan.
type DirectoryListing struct {
	InodeNumber uint64 `json:"inode_number"`
	// Entries holds active entries only, in on-disk order.
	Entries []DirectoryEntry `json:"entries,omitempty"`
	// Records holds every record produced by the scan, including free slots
	// and carved candidates when IncludeDeleted is set.
	Records []DirectoryRecord `json:"records,omitempty"`
	// Anomalies records structural problems encountered in best-effort mode.
	Anomalies []ReportAnomaly `json:"anomalies,omitempty"`
	// Truncated reports that a cap was reached and results are incomplete.
	Truncated bool `json:"truncated,omitempty"`
	// BlocksScanned counts directory blocks actually read.
	BlocksScanned uint64 `json:"blocks_scanned,omitempty"`
	// Format names the directory layout that was parsed.
	Format string `json:"format,omitempty"`
}

// Directory layout names reported in DirectoryListing.Format.
const (
	DirectoryFormatShortForm  = "short_form"
	DirectoryFormatBlock      = "block"
	DirectoryFormatMultiBlock = "multi_block"
)

// ListDirectoryEntriesWithOptions lists a directory under explicit scan options.
func (v *Volume) ListDirectoryEntriesWithOptions(inodeNumber uint64, options DirectoryScanOptions) (DirectoryListing, error) {
	options.IncludeDeleted = false
	return v.scanDirectory(inodeNumber, options)
}

// ScanDirectoryRecordsWithOptions scans a directory for active, free and
// carved records under explicit scan options.
func (v *Volume) ScanDirectoryRecordsWithOptions(inodeNumber uint64, options DirectoryScanOptions) (DirectoryListing, error) {
	options.IncludeDeleted = true
	return v.scanDirectory(inodeNumber, options)
}

// ScanDirectoryRecordsByPathWithOptions resolves a path and scans it.
func (v *Volume) ScanDirectoryRecordsByPathWithOptions(path string, options DirectoryScanOptions) (DirectoryListing, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return DirectoryListing{}, err
	}
	return v.ScanDirectoryRecordsWithOptions(inodeNumber, options)
}

// directoryBlockSize returns the directory block size, falling back to the
// filesystem block size when the superblock did not specify one.
func (v *Volume) directoryBlockSize() uint32 {
	if v.ioh.directoryBlockSize != 0 {
		return v.ioh.directoryBlockSize
	}
	return v.ioh.blockSize
}

// validateDirectoryBlockSize checks the directory block geometry before it is
// used to frame anything.
//
// XFS derives the directory block size as sb_blocksize << sb_dirblklog, so it
// is always a power of two, never smaller than the filesystem block size, and
// never larger than XFS_MAX_BLOCKSIZE.
func (v *Volume) validateDirectoryBlockSize() (uint64, error) {
	size := v.directoryBlockSize()
	if size == 0 {
		return 0, wrapParseError(0, "directory_block_size", ErrInvalidSuperblock)
	}
	if size > maxBlockSize {
		return 0, wrapParseError(int64(size), "directory_block_size", ErrInvalidSuperblock)
	}
	if size&(size-1) != 0 {
		return 0, wrapParseError(int64(size), "directory_block_size", ErrInvalidSuperblock)
	}
	if v.ioh.blockSize != 0 && size < v.ioh.blockSize {
		return 0, wrapParseError(int64(size), "directory_block_size", ErrInvalidSuperblock)
	}
	return uint64(size), nil
}

// readDirectoryBlock reads a single directory block from a directory inode.
//
// Allocation is always one directory block, never the recorded directory size,
// so a crafted di_size cannot drive an out-of-memory condition.
func (v *Volume) readDirectoryBlock(inode *Inode, blockIndex uint64) ([]byte, error) {
	directoryBlockSize, err := v.validateDirectoryBlockSize()
	if err != nil {
		return nil, err
	}

	blockOffset := blockIndex * directoryBlockSize
	if blockOffset >= inode.Size {
		return nil, wrapParseError(int64(blockOffset), "directory_block_offset", ErrInvalidInode)
	}

	readSize := directoryBlockSize
	if blockOffset+readSize > inode.Size {
		readSize = inode.Size - blockOffset
	}

	buffer := make([]byte, readSize)
	n, err := v.readInodeData(inode, buffer, int64(blockOffset))
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n == 0 {
		return nil, wrapParseError(int64(blockOffset), "directory_block", ErrInvalidInode)
	}
	return buffer[:n], nil
}

// scanDirectory is the single implementation behind every directory listing
// and record scanning API.
func (v *Volume) scanDirectory(inodeNumber uint64, options DirectoryScanOptions) (DirectoryListing, error) {
	listing := DirectoryListing{InodeNumber: inodeNumber}

	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return listing, err
	}
	if !inode.IsDirectory() {
		return listing, wrapParseError(0, "directory_inode", ErrInvalidInode)
	}

	entries, err := parseShortFormDirectoryEntries(inode, v.ioh.formatVersion, v.ioh.secondaryFeatureFlags)
	if err == nil {
		listing.Format = DirectoryFormatShortForm
		listing.Entries = entries
		listing.Records = make([]DirectoryRecord, 0, len(entries))
		for _, entry := range entries {
			listing.Records = append(listing.Records, DirectoryRecord{
				Name:              entry.Name,
				InodeNumber:       entry.InodeNumber,
				FileType:          entry.FileType,
				Kind:              RecordKindActive,
				Confidence:        ConfidenceHigh,
				ConfidenceReasons: []string{ReasonIntactFraming},
			})
		}
		return listing, nil
	}
	if !errors.Is(err, ErrUnsupportedDirFormat) {
		return listing, err
	}

	return v.scanBlockDirectory(inode, listing, options)
}

// scanBlockDirectory walks the data-block region of a directory.
//
// XFS reserves the first 32 GiB of a directory's logical space for data
// blocks, and only advances di_size for that region, so iterating
// [0, di_size) in directory-block steps enumerates every entry regardless of
// whether the directory is in block, leaf, node or btree format. Leaf and
// node blocks live above the boundary and are never visited here.
func (v *Volume) scanBlockDirectory(inode *Inode, listing DirectoryListing, options DirectoryScanOptions) (DirectoryListing, error) {
	directoryBlockSize, err := v.validateDirectoryBlockSize()
	if err != nil {
		return listing, err
	}

	total := inode.Size
	if total > dirLeafSpaceOffset {
		listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
			Code:     "directory_size_exceeds_data_space",
			Severity: "warning",
			Inode:    listing.InodeNumber,
			Message: fmt.Sprintf("directory size %d exceeds the %d byte data space; truncating the walk",
				total, dirLeafSpaceOffset),
		})
		total = dirLeafSpaceOffset
	}
	if capacity := v.volumeCapacityBytes(); capacity != 0 && total > capacity {
		listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
			Code:     "directory_size_exceeds_volume",
			Severity: "warning",
			Inode:    listing.InodeNumber,
			Message: fmt.Sprintf("directory size %d exceeds the %d byte volume capacity; truncating the walk",
				inode.Size, capacity),
		})
		total = capacity
	}
	if total == 0 {
		if !options.BestEffort {
			return listing, wrapParseError(0, "directory_data", ErrInvalidInode)
		}
		listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
			Code:     "directory_empty",
			Severity: "warning",
			Inode:    listing.InodeNumber,
			Message:  "directory has no data blocks",
		})
		return listing, nil
	}

	blockCount := total / directoryBlockSize
	if total%directoryBlockSize != 0 {
		blockCount++
		listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
			Code:     "directory_size_unaligned",
			Severity: "info",
			Inode:    listing.InodeNumber,
			Message: fmt.Sprintf("directory size %d is not a multiple of the %d byte directory block size",
				inode.Size, directoryBlockSize),
		})
	}

	if blockCount == 1 {
		listing.Format = DirectoryFormatBlock
	} else {
		listing.Format = DirectoryFormatMultiBlock
	}

	maxBlocks := options.maxBlocks()
	if blockCount > maxBlocks {
		blockCount = maxBlocks
		listing.Truncated = true
	}
	maxEntries := options.maxEntries()

	buffer := make([]byte, directoryBlockSize)

	for blockIndex := uint64(0); blockIndex < blockCount; blockIndex++ {
		blockOffset := blockIndex * directoryBlockSize

		readSize := directoryBlockSize
		if blockOffset+readSize > total {
			readSize = total - blockOffset
		}

		n, readErr := v.readInodeData(inode, buffer[:readSize], int64(blockOffset))
		if readErr != nil && readErr != io.EOF {
			if !options.BestEffort {
				return listing, readErr
			}
			listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
				Code:     "directory_block_read_failed",
				Severity: "error",
				Inode:    listing.InodeNumber,
				Message:  fmt.Sprintf("directory block %d: %v", blockIndex, readErr),
			})
			break
		}
		if n == 0 {
			break
		}
		listing.BlocksScanned++

		block := buffer[:n]
		kind, _ := classifyDirectoryBlock(block)
		switch kind {
		case dirBlockKindData, dirBlockKindBlock:
		case dirBlockKindEmpty:
			// Freed data blocks are punched out of the directory's logical
			// space, so an all-zero block is a normal hole.
			continue
		default:
			anomaly := ReportAnomaly{
				Code:     "directory_unexpected_block",
				Severity: "warning",
				Inode:    listing.InodeNumber,
				Message: fmt.Sprintf("directory block %d holds a %s block inside the data space",
					blockIndex, kind),
			}
			if !options.BestEffort {
				return listing, fmt.Errorf("%w: %s", ErrUnsupportedDirFormat, anomaly.Message)
			}
			listing.Anomalies = append(listing.Anomalies, anomaly)
			continue
		}

		records, anomalies, err := parseDirectoryBlockRecords(block, directoryParseContext{
			hasFileType:    directoryHasFileType(v.ioh.formatVersion, v.ioh.secondaryFeatureFlags),
			includeDeleted: options.IncludeDeleted,
			bestEffort:     options.BestEffort,
			blockIndex:     blockIndex,
			blockOffset:    blockOffset,
		})
		for i := range anomalies {
			anomalies[i].Inode = listing.InodeNumber
		}
		listing.Anomalies = append(listing.Anomalies, anomalies...)
		if err != nil {
			if !options.BestEffort {
				return listing, err
			}
			listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
				Code:     "directory_block_parse_failed",
				Severity: "error",
				Inode:    listing.InodeNumber,
				Message:  fmt.Sprintf("directory block %d: %v", blockIndex, err),
			})
			continue
		}

		for _, record := range records {
			if len(listing.Records) >= maxEntries {
				listing.Truncated = true
				return listing, nil
			}
			listing.Records = append(listing.Records, record)
			if record.Kind == RecordKindActive {
				listing.Entries = append(listing.Entries, DirectoryEntry{
					Name:        record.Name,
					InodeNumber: record.InodeNumber,
					FileType:    record.FileType,
				})
			}
		}
	}

	return listing, nil
}
