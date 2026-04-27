package libxfs

import "fmt"

const (
	extentLogicalBlockMask  = uint64(0x3fffffffffffff)
	extentStartBlockLowMask = uint64(0x7ffffffffff)
)

func parseExtent(data []byte) (*Extent, error) {
	if len(data) < 16 {
		return nil, wrapParseError(0, "extent", ErrInvalidInode)
	}

	valueUpper, ok := readUint64BE(data, 0)
	if !ok {
		return nil, wrapParseError(0, "extent_upper", ErrInvalidInode)
	}
	valueLower, ok := readUint64BE(data, 8)
	if !ok {
		return nil, wrapParseError(8, "extent_lower", ErrInvalidInode)
	}

	numberOfBlocks := uint32(valueLower & 0x1fffff)
	startBlockLow := (valueLower >> 21) & extentStartBlockLowMask
	startBlockHigh := valueUpper & 0x1ff
	physicalBlockNumber := (startBlockHigh << 43) | startBlockLow

	logicalBlockNumber := (valueUpper >> 9) & extentLogicalBlockMask

	valueUpper >>= 63

	rangeFlags := uint32(0)
	if valueUpper != 0 {
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
	if len(data) < 16 {
		return 0
	}

	count := uint32(0)
	for off := 0; off+16 <= len(data); off += 16 {
		upper, okUpper := readUint64BE(data, off)
		lower, okLower := readUint64BE(data, off+8)
		if !okUpper || !okLower {
			break
		}
		if upper == 0 && lower == 0 {
			break
		}

		numberOfBlocks := uint32(lower & 0x1fffff)
		if numberOfBlocks == 0 {
			break
		}

		count++
	}
	return count
}

func parseExtentList(numberOfBlocks uint64, numberOfExtents uint32, data []byte, addSparseExtents bool) ([]Extent, error) {
	if int(numberOfExtents) > len(data)/16 {
		return nil, wrapParseError(0, "number_of_extents", ErrInvalidInode)
	}

	extents := make([]Extent, 0, numberOfExtents+1)
	logicalBlockNumber := uint64(0)

	for i := uint32(0); i < numberOfExtents; i++ {
		off := int(i) * 16
		extent, err := parseExtent(data[off : off+16])
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
