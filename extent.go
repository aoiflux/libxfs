package libxfs

import "fmt"

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

	rangeFlags := uint32(0)
	if valueUpper>>extentUnwrittenShift != 0 {
		rangeFlags = ExtentFlagSparse
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

func parseExtentList(numberOfBlocks uint64, numberOfExtents uint32, data []byte, addSparseExtents bool) ([]Extent, error) {
	if int(numberOfExtents) > len(data)/extentRecordSize {
		return nil, wrapParseError(0, "number_of_extents", ErrInvalidInode)
	}

	extents := make([]Extent, 0, numberOfExtents+1)
	logicalBlockNumber := uint64(0)

	for i := uint32(0); i < numberOfExtents; i++ {
		off := int(i) * extentRecordSize
		extent, err := parseExtent(data[off : off+extentRecordSize])
		if err != nil {
			return nil, fmt.Errorf("extent[%d]: %w", i, err)
		}

		if addSparseExtents && extent.LogicalBlockNumber > logicalBlockNumber {
			extents = append(extents, Extent{
				LogicalBlockNumber: logicalBlockNumber,
				NumberOfBlocks:     uint32(extent.LogicalBlockNumber - logicalBlockNumber),
				RangeFlags:         ExtentFlagSparse,
			})
		}

		extents = append(extents, *extent)
		logicalBlockNumber = extent.LogicalBlockNumber + uint64(extent.NumberOfBlocks)
	}

	if addSparseExtents && logicalBlockNumber < numberOfBlocks {
		if len(extents) == 0 || (extents[len(extents)-1].RangeFlags&ExtentFlagSparse) == 0 {
			extents = append(extents, Extent{
				LogicalBlockNumber: logicalBlockNumber,
				RangeFlags:         ExtentFlagSparse,
			})
		}
		last := &extents[len(extents)-1]
		last.NumberOfBlocks += uint32(numberOfBlocks - logicalBlockNumber)
	}

	return extents, nil
}
