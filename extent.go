package libxfs

import (
	"fmt"
	"sort"
)

func parseExtent(data []byte) (*Extent, error) {
	const upperWordOffset, lowerWordOffset = 0, 8

	if len(data) < extentRecordSize {
		return nil, wrapParseError(0, "extent", ErrInvalidInode)
	}

	valueUpper, ok := readUint64BE(data, upperWordOffset)
	if !ok {
		return nil, wrapParseError(upperWordOffset, "extent_upper", ErrInvalidInode)
	}
	valueLower, ok := readUint64BE(data, lowerWordOffset)
	if !ok {
		return nil, wrapParseError(lowerWordOffset, "extent_lower", ErrInvalidInode)
	}

	numberOfBlocks := uint32(valueLower & extentBlockCountMask)
	startBlockLow := (valueLower >> extentBlockCountBits) & extentStartLowMask
	startBlockHigh := valueUpper & extentStartHighMask
	physicalBlockNumber := (startBlockHigh << extentStartLowBits) | startBlockLow

	logicalBlockNumber := (valueUpper >> extentLogicalBlockPos) & extentLogicalMask

	// The unwritten bit means the blocks are allocated but were never written,
	// so the range reads as zeros like a hole while still naming real blocks.
	// Both facts are recorded: reporting it only as sparse loses the location,
	// and reporting it only as unwritten would make readers return whatever
	// bytes happen to be on the medium.
	rangeFlags := uint32(0)
	if valueUpper>>extentUnwrittenShift != 0 {
		rangeFlags = ExtentFlagSparse | ExtentFlagUnwritten
	}

	return &Extent{
		LogicalBlockNumber:  logicalBlockNumber,
		PhysicalBlockNumber: physicalBlockNumber,
		NumberOfBlocks:      numberOfBlocks,
		RangeFlags:          rangeFlags,
	}, nil
}

func inferExtentCount(data []byte) uint32 {
	if len(data) < extentRecordSize {
		return 0
	}

	count := uint32(0)
	for off := 0; off+extentRecordSize <= len(data); off += extentRecordSize {
		upper, okUpper := readUint64BE(data, off)
		lower, okLower := readUint64BE(data, off+8)
		if !okUpper || !okLower {
			break
		}
		if upper == 0 && lower == 0 {
			break
		}

		numberOfBlocks := uint32(lower & extentBlockCountMask)
		if numberOfBlocks == 0 {
			break
		}

		count++
	}
	return count
}

// parseExtentList decodes a run of on-disk extent records exactly as written.
//
// It performs no hole synthesis. A b+tree-mapped fork is assembled from many
// leaf blocks, and a function that only ever sees one block cannot know where
// the holes are: it would treat the start of every leaf after the first as a
// gap from logical block zero, producing sparse runs that overlap extents
// already decoded from earlier leaves. Holes are filled once, by
// fillSparseExtents, after the whole fork has been collected.
func parseExtentList(numberOfExtents uint32, data []byte) ([]Extent, error) {
	if int(numberOfExtents) > len(data)/extentRecordSize {
		return nil, wrapParseError(0, "number_of_extents", ErrInvalidInode)
	}

	extents := make([]Extent, 0, numberOfExtents)
	for i := uint32(0); i < numberOfExtents; i++ {
		off := int(i) * extentRecordSize
		extent, err := parseExtent(data[off : off+extentRecordSize])
		if err != nil {
			return nil, fmt.Errorf("extent[%d]: %w", i, err)
		}
		extents = append(extents, *extent)
	}
	return extents, nil
}

// normalizeExtents makes a fork's extent list a well-formed mapping: sorted by
// logical block, with no overlaps and no zero-length records.
//
// XFS already writes extents sorted and disjoint, so this is a no-op for an
// intact image. It matters for a crafted or damaged one, because lookups
// binary search the list, and a binary search over overlapping records can
// return a different extent than a scan would. Where records do overlap the
// earlier one wins and the later one is clipped, which preserves the
// first-match behaviour a linear scan had.
func normalizeExtents(extents []Extent) []Extent {
	if !sort.SliceIsSorted(extents, func(i, j int) bool {
		return extents[i].LogicalBlockNumber < extents[j].LogicalBlockNumber
	}) {
		sort.SliceStable(extents, func(i, j int) bool {
			return extents[i].LogicalBlockNumber < extents[j].LogicalBlockNumber
		})
	}

	normalized := make([]Extent, 0, len(extents))
	previousEnd := uint64(0)

	for _, extent := range extents {
		// A zero-length record maps nothing. Keeping it would put an entry in
		// the list that no block can ever resolve to, which is exactly the
		// kind of record a binary search can land on.
		if extent.NumberOfBlocks == 0 {
			continue
		}
		if extent.LogicalBlockNumber < previousEnd {
			overlap := previousEnd - extent.LogicalBlockNumber
			if overlap >= uint64(extent.NumberOfBlocks) {
				continue
			}
			extent.LogicalBlockNumber += overlap
			// A hole has no physical location to advance; only a range that
			// names real blocks does.
			if extent.PhysicalBlockNumber != 0 {
				extent.PhysicalBlockNumber += overlap
			}
			extent.NumberOfBlocks -= uint32(overlap)
		}
		normalized = append(normalized, extent)
		previousEnd = extent.LogicalBlockNumber + uint64(extent.NumberOfBlocks)
	}
	return normalized
}

// fillSparseExtents inserts explicit sparse runs for the gaps in a fork.
//
// extents must already have been through normalizeExtents. numberOfBlocks is
// the fork's logical length, so that a file ending in a hole gets one too.
func fillSparseExtents(extents []Extent, numberOfBlocks uint64) []Extent {
	filled := make([]Extent, 0, len(extents)+1)
	logicalBlockNumber := uint64(0)

	for _, extent := range extents {
		if extent.LogicalBlockNumber > logicalBlockNumber {
			filled = append(filled, Extent{
				LogicalBlockNumber: logicalBlockNumber,
				NumberOfBlocks:     uint32(extent.LogicalBlockNumber - logicalBlockNumber),
				RangeFlags:         ExtentFlagSparse,
			})
		}
		filled = append(filled, extent)
		logicalBlockNumber = extent.LogicalBlockNumber + uint64(extent.NumberOfBlocks)
	}

	if logicalBlockNumber < numberOfBlocks {
		filled = append(filled, Extent{
			LogicalBlockNumber: logicalBlockNumber,
			NumberOfBlocks:     uint32(numberOfBlocks - logicalBlockNumber),
			RangeFlags:         ExtentFlagSparse,
		})
	}
	return filled
}
