package libxfs

import (
	"errors"
	"fmt"
	"io"
	"math"
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

	// includeDotEntries keeps the "." and ".." records, which every other
	// caller wants filtered out. Only index verification needs them, because
	// XFS indexes them alongside every other name, and a comparison that
	// dropped them from one side would report every intact directory as
	// divergent.
	includeDotEntries bool
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
	// Records holds every record produced by a forensic scan: active entries,
	// free slots and carved candidates. It is populated by the
	// ScanDirectoryRecords* APIs. A plain listing leaves it empty, since
	// building it would double the cost of the common path; use Entries there.
	Records []DirectoryRecord `json:"records,omitempty"`
	// Anomalies records structural problems encountered in best-effort mode.
	Anomalies []ReportAnomaly `json:"anomalies,omitempty"`
	// Truncated reports that a cap was reached and results are incomplete.
	Truncated bool `json:"truncated,omitempty"`
	// BlocksScanned counts directory blocks actually read.
	BlocksScanned uint64 `json:"blocks_scanned,omitempty"`
	// Format names the directory layout that was parsed. It distinguishes only
	// what the walker had to do: a single block, or several. Use SourceFormat
	// to learn the directory's actual on-disk shape.
	Format string `json:"format,omitempty"`
	// SourceFormat names the directory's on-disk index format: short_form,
	// block, leaf or node.
	//
	// It is derived from the inode's fork type and from where the directory's
	// blocks sit in its logical space, not from how many data blocks it has.
	// Two directories with the same data-block count can be in different
	// formats, so Format cannot answer this and must not be used to.
	SourceFormat string `json:"source_format,omitempty"`
}

// Directory layout names reported in DirectoryListing.Format.
const (
	DirectoryFormatShortForm  = "short_form"
	DirectoryFormatBlock      = "block"
	DirectoryFormatMultiBlock = "multi_block"
)

// On-disk directory index formats reported in DirectoryListing.SourceFormat.
const (
	DirectorySourceFormatShortForm = "short_form"
	DirectorySourceFormatBlock     = "block"
	DirectorySourceFormatLeaf      = "leaf"
	DirectorySourceFormatNode      = "node"
)

// directorySourceFormat determines a directory's on-disk index format.
//
// XFS divides a directory's logical space into 32 GiB regions: data blocks in
// the first, the leaf and node index in the second, free-space bitmaps in the
// third. A directory with nothing in the index region is in block format; one
// whose index is a single directory block is in leaf format; anything larger
// needs a da-node above the leaves.
func (v *Volume) directorySourceFormat(inode *Inode) string {
	if inode.ForkType == ForkTypeInlineData {
		return DirectorySourceFormatShortForm
	}
	blockSize := uint64(v.ioh.blockSize)
	directoryBlockSize := uint64(v.directoryBlockSize())
	if blockSize == 0 || directoryBlockSize == 0 {
		return ""
	}

	leafRegionStart := dirLeafSpaceOffset / blockSize
	freeRegionStart := dirFreeSpaceOffset / blockSize

	var leafBlocks, freeBlocks uint64
	for _, extent := range inode.DataExtents {
		if extent.RangeFlags&ExtentFlagSparse != 0 {
			continue
		}
		switch {
		case extent.LogicalBlockNumber >= freeRegionStart:
			freeBlocks += uint64(extent.NumberOfBlocks)
		case extent.LogicalBlockNumber >= leafRegionStart:
			leafBlocks += uint64(extent.NumberOfBlocks)
		}
	}

	switch {
	case leafBlocks == 0 && freeBlocks == 0:
		return DirectorySourceFormatBlock
	case freeBlocks == 0 && leafBlocks*blockSize <= directoryBlockSize:
		return DirectorySourceFormatLeaf
	default:
		return DirectorySourceFormatNode
	}
}

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

// inodeAllocationState reports whether an inode number is structurally usable
// on this filesystem, and if so whether the inode b-tree considers it
// allocated.
//
// A carved candidate's inode number is just bytes recovered from reclaimed
// space, so a number that cannot address anything on this volume is strong
// evidence that the match is a coincidence.
func (v *Volume) inodeAllocationState(inodeNumber uint64) (addressable bool, allocated bool) {
	if inodeNumber == 0 || inodeNumber > math.MaxUint32 {
		return false, false
	}
	bits := v.ioh.relativeInodeNumberBits
	if bits == 0 || bits >= maxRelativeInodeBits {
		return false, false
	}

	allocationGroupIndex := int(inodeNumber >> bits)
	relativeInodeNumber := inodeNumber & ((uint64(1) << bits) - 1)
	if allocationGroupIndex < 0 || allocationGroupIndex >= len(v.agInode) {
		return false, false
	}

	found, err := v.hasRelativeInodeInBtree(allocationGroupIndex, relativeInodeNumber)
	if err != nil {
		// The number is addressable; we simply could not confirm its state.
		return true, false
	}
	return true, found
}

// refineCarvedRecord adjusts a carved candidate's confidence using context the
// block parser does not have: whether the recovered inode number can address
// anything on this volume, and what the inode b-tree says about it.
//
// A deleted entry's inode is usually freed, so "unallocated" is consistent
// with genuine deletion and "allocated" may mean the number has since been
// reused. Neither is treated as proof; both are recorded as evidence.
func (v *Volume) refineCarvedRecord(record *DirectoryRecord) {
	addressable, allocated := v.inodeAllocationState(record.InodeNumber)

	if !addressable {
		record.Confidence = ConfidenceLow
		record.ConfidenceReasons = append(record.ConfidenceReasons, ReasonInodeUnaddressable)
		return
	}
	if allocated {
		record.ConfidenceReasons = append(record.ConfidenceReasons, ReasonInodeAllocated)
	} else {
		record.ConfidenceReasons = append(record.ConfidenceReasons, ReasonInodeUnallocated)
	}
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

	listing.SourceFormat = v.directorySourceFormat(inode)

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
	collected := 0

	// firstError holds the first failure met while walking the blocks.
	//
	// A directory is a sequence of independently framed blocks, so damage to
	// one says nothing about the others. Returning at the first failure throws
	// away every entry in every later block, which on a recursive walk silently
	// discards the whole subtree. Instead every block is attempted, and the
	// original error is returned at the end alongside everything recovered.
	var firstError error
	recordError := func(err error) {
		if firstError == nil {
			firstError = err
		}
	}

	buffer := make([]byte, directoryBlockSize)

	for blockIndex := uint64(0); blockIndex < blockCount; blockIndex++ {
		blockOffset := blockIndex * directoryBlockSize

		readSize := directoryBlockSize
		if blockOffset+readSize > total {
			readSize = total - blockOffset
		}

		n, readErr := v.readInodeData(inode, buffer[:readSize], int64(blockOffset))
		if readErr != nil && readErr != io.EOF {
			recordError(readErr)
			listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
				Code:     "directory_block_read_failed",
				Severity: "error",
				Inode:    listing.InodeNumber,
				Message:  fmt.Sprintf("directory block %d: %v", blockIndex, readErr),
			})
			// An unreadable block is a hole in the evidence, not the end of it.
			// Later blocks are mapped independently and are usually readable.
			continue
		}
		if n == 0 {
			continue
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
			listing.Anomalies = append(listing.Anomalies, anomaly)
			if !options.BestEffort {
				recordError(fmt.Errorf("%w: %s", ErrUnsupportedDirFormat, anomaly.Message))
			}
			continue
		}

		records, anomalies, err := parseDirectoryBlockRecords(block, directoryParseContext{
			hasFileType:          directoryHasFileType(v.ioh.formatVersion, v.ioh.secondaryFeatureFlags),
			includeDeleted:       options.IncludeDeleted,
			bestEffort:           options.BestEffort,
			includeDotEntries:    options.includeDotEntries,
			explainActiveRecords: options.IncludeDeleted,
			blockIndex:           blockIndex,
			blockOffset:          blockOffset,
		})
		for i := range anomalies {
			anomalies[i].Inode = listing.InodeNumber
		}
		listing.Anomalies = append(listing.Anomalies, anomalies...)
		if err != nil {
			recordError(err)
			listing.Anomalies = append(listing.Anomalies, ReportAnomaly{
				Code:     "directory_block_parse_failed",
				Severity: "error",
				Inode:    listing.InodeNumber,
				Message:  fmt.Sprintf("directory block %d: %v", blockIndex, err),
			})
			continue
		}

		for _, record := range records {
			if collected >= maxEntries {
				listing.Truncated = true
				return listing, scanError(listing, options, firstError)
			}
			collected++

			// A plain listing never surfaces Records, so building them would
			// double the allocation for the hottest path in the library.
			if options.IncludeDeleted {
				if record.Kind == RecordKindCarved {
					v.refineCarvedRecord(&record)
				}
				listing.Records = append(listing.Records, record)
			}
			if record.Kind == RecordKindActive {
				listing.Entries = append(listing.Entries, DirectoryEntry{
					Name:        record.Name,
					InodeNumber: record.InodeNumber,
					FileType:    record.FileType,
				})
			}
		}
	}

	return listing, scanError(listing, options, firstError)
}

// scanError reports the outcome of a completed block walk.
//
// A real failure is reported as itself, so that callers matching on
// ErrInvalidInode or ErrUnsupportedDirFormat keep working.
//
// Truncation is an error only when the cap that fired was the package default.
// A caller that set MaxBlocks or MaxEntries asked to be cut off and reads
// Truncated to find out; a caller that set neither does not know a cap exists,
// and would otherwise present a partial directory as the whole thing.
func scanError(listing DirectoryListing, options DirectoryScanOptions, firstError error) error {
	if firstError != nil {
		return firstError
	}
	if listing.Truncated && options.MaxBlocks == 0 && options.MaxEntries == 0 {
		return fmt.Errorf("%w: inode %d stopped after %d blocks and %d entries at the default limit",
			ErrDirectoryTruncated, listing.InodeNumber, listing.BlocksScanned, len(listing.Entries))
	}
	return nil
}
