package libxfs

import "fmt"

func parseShortFormAttributes(data []byte) ([]ExtendedAttribute, error) {
	if len(data) < 4 {
		return nil, wrapParseError(0, "xattr_shortform_header", ErrInvalidAttributeData)
	}

	dataSize, ok := readUint16BE(data, 0)
	if !ok {
		return nil, wrapParseError(0, "xattr_data_size", ErrInvalidAttributeData)
	}
	numberOfEntries := int(data[2])
	if int(dataSize) < 4 || int(dataSize) > len(data) {
		return nil, wrapParseError(0, "xattr_data_size", ErrInvalidAttributeData)
	}

	offset := 4
	attributes := make([]ExtendedAttribute, 0, numberOfEntries)

	for i := 0; i < numberOfEntries; i++ {
		if offset+3 > int(dataSize) {
			return nil, wrapParseError(int64(offset), "xattr_entry_header", ErrInvalidAttributeData)
		}

		nameSize := int(data[offset])
		valueSize := int(data[offset+1])
		flags := data[offset+2]
		offset += 3

		if offset+nameSize > int(dataSize) {
			return nil, wrapParseError(int64(offset), "xattr_name", ErrInvalidAttributeData)
		}
		name := string(data[offset : offset+nameSize])
		offset += nameSize

		if offset+valueSize > int(dataSize) {
			return nil, wrapParseError(int64(offset), "xattr_value", ErrInvalidAttributeData)
		}
		value := append([]byte(nil), data[offset:offset+valueSize]...)
		offset += valueSize

		prefix, namespace, err := xattrNamespaceFromFlags(flags)
		if err != nil {
			return nil, fmt.Errorf("xattr[%d]: %w", i, err)
		}

		attributes = append(attributes, ExtendedAttribute{
			Name:      prefix + name,
			Namespace: namespace,
			Value:     value,
			Flags:     flags,
		})
	}

	return attributes, nil
}

func xattrNamespaceFromFlags(flags uint8) (prefix string, namespace string, err error) {
	switch flags & 0x7e {
	case 0:
		return "user.", "user", nil
	case 2:
		return "trusted.", "trusted", nil
	case 4:
		return "secure.", "secure", nil
	default:
		return "", "", wrapParseError(0, "xattr_flags", ErrUnsupportedXattrFormat)
	}
}

func (v *Volume) parseAttributesFromBlocks(inode *Inode) ([]ExtendedAttribute, error) {
	if inode == nil {
		return nil, wrapParseError(0, "inode", ErrInvalidInode)
	}
	if len(inode.AttributesExtents) == 0 {
		return nil, nil
	}

	return v.parseAttributesFromBlockNumber(inode, 0, 0)
}

func (v *Volume) parseAttributesFromBlockNumber(inode *Inode, blockNumber uint32, recursionDepth int) ([]ExtendedAttribute, error) {
	if recursionDepth < 0 || recursionDepth > maxBtreeRecursionDepth {
		return nil, wrapParseError(0, "xattr_block_recursion_depth", ErrInvalidAttributeData)
	}

	blockData, err := v.readAttributesLogicalBlock(inode, blockNumber)
	if err != nil {
		return nil, err
	}

	if len(blockData) < 12 {
		return nil, wrapParseError(0, "xattr_block_header", ErrInvalidAttributeData)
	}

	sig, ok := readUint16BE(blockData, 8)
	if !ok {
		return nil, wrapParseError(8, "xattr_block_signature", ErrInvalidAttributeData)
	}

	if sig == 0x3bee || sig == 0xfbee {
		return v.parseLeafBlockAttributes(inode, blockData)
	}
	if sig == 0x3ebe || sig == 0xfebe {
		subBlocks, err := parseBranchBlockPointers(blockData, v.ioh.formatVersion)
		if err != nil {
			return nil, err
		}
		out := make([]ExtendedAttribute, 0)
		for _, sub := range subBlocks {
			attrs, err := v.parseAttributesFromBlockNumber(inode, sub, recursionDepth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, attrs...)
		}
		return out, nil
	}

	return nil, wrapParseError(8, "xattr_block_signature", ErrUnsupportedXattrFormat)
}

func (v *Volume) readAttributesLogicalBlock(inode *Inode, blockNumber uint32) ([]byte, error) {
	extent := findAttributeExtentForLogicalBlock(inode.AttributesExtents, uint64(blockNumber))
	if extent == nil {
		return nil, wrapParseError(0, "xattr_block_number", ErrInvalidAttributeData)
	}

	if v.ioh.blockSize == 0 || v.ioh.allocationGroupSize == 0 {
		return nil, wrapParseError(0, "io_geometry", ErrInvalidSuperblock)
	}

	allocationGroupIndex := extent.PhysicalBlockNumber >> v.ioh.relativeBlockNumberBits
	relativeBlockNumber := extent.PhysicalBlockNumber & ((uint64(1) << v.ioh.relativeBlockNumberBits) - 1)

	offBlocks := allocationGroupIndex*uint64(v.ioh.allocationGroupSize) + relativeBlockNumber + (uint64(blockNumber) - extent.LogicalBlockNumber)
	if offBlocks > uint64(^uint(0)>>1)/uint64(v.ioh.blockSize) {
		return nil, wrapParseError(0, "xattr_block_offset", ErrInvalidAttributeData)
	}

	offset := int64(offBlocks * uint64(v.ioh.blockSize))
	buf := make([]byte, v.ioh.blockSize)
	if err := readAtFull(v.reader, buf, offset); err != nil {
		return nil, wrapIOError("read", offset, len(buf), err)
	}
	return buf, nil
}

func findAttributeExtentForLogicalBlock(extents []Extent, blockNumber uint64) *Extent {
	for i := range extents {
		extent := &extents[i]
		if blockNumber >= extent.LogicalBlockNumber && blockNumber < extent.LogicalBlockNumber+uint64(extent.NumberOfBlocks) {
			return extent
		}
	}
	return nil
}

func parseBranchBlockPointers(data []byte, formatVersion uint8) ([]uint32, error) {
	fsHeaderSize := 12
	branchHeaderSize := 4
	if formatVersion == 5 {
		fsHeaderSize = 48
		branchHeaderSize = 8
	}
	if len(data) < fsHeaderSize+branchHeaderSize {
		return nil, wrapParseError(0, "xattr_branch_header", ErrInvalidAttributeData)
	}

	numberOfEntries, ok := readUint16BE(data, fsHeaderSize)
	if !ok {
		return nil, wrapParseError(int64(fsHeaderSize), "xattr_branch_entries", ErrInvalidAttributeData)
	}

	entriesOffset := fsHeaderSize + branchHeaderSize
	entriesSize := int(numberOfEntries) * 8
	if entriesOffset+entriesSize > len(data) {
		return nil, wrapParseError(int64(entriesOffset), "xattr_branch_entries_size", ErrInvalidAttributeData)
	}

	out := make([]uint32, 0, numberOfEntries)
	for i := 0; i < int(numberOfEntries); i++ {
		off := entriesOffset + i*8 + 4
		subBlock, ok := readUint32BE(data, off)
		if !ok {
			return nil, wrapParseError(int64(off), "xattr_branch_sub_block", ErrInvalidAttributeData)
		}
		out = append(out, subBlock)
	}
	return out, nil
}

func (v *Volume) parseLeafBlockAttributes(inode *Inode, data []byte) ([]ExtendedAttribute, error) {
	formatVersion := v.ioh.formatVersion
	fsHeaderSize := 12
	leafHeaderSize := 20
	if formatVersion == 5 {
		fsHeaderSize = 48
		leafHeaderSize = 24
	}
	if len(data) < fsHeaderSize+leafHeaderSize {
		return nil, wrapParseError(0, "xattr_leaf_header", ErrInvalidAttributeData)
	}

	numberOfEntries, ok := readUint16BE(data, fsHeaderSize)
	if !ok {
		return nil, wrapParseError(int64(fsHeaderSize), "xattr_leaf_entries", ErrInvalidAttributeData)
	}

	entriesOffset := fsHeaderSize + leafHeaderSize
	entriesSize := int(numberOfEntries) * 8
	entriesEnd := entriesOffset + entriesSize
	if entriesEnd > len(data) {
		return nil, wrapParseError(int64(entriesOffset), "xattr_leaf_entries_size", ErrInvalidAttributeData)
	}

	out := make([]ExtendedAttribute, 0, numberOfEntries)
	for i := 0; i < int(numberOfEntries); i++ {
		entryOff := entriesOffset + i*8
		valuesOffset16, ok := readUint16BE(data, entryOff+4)
		if !ok {
			return nil, wrapParseError(int64(entryOff+4), "xattr_leaf_values_offset", ErrInvalidAttributeData)
		}
		flags := data[entryOff+6]
		valuesOffset := int(valuesOffset16)
		if valuesOffset < entriesEnd || valuesOffset >= len(data) {
			return nil, wrapParseError(int64(valuesOffset), "xattr_leaf_values_offset", ErrInvalidAttributeData)
		}

		isLocal := (flags & 0x01) != 0
		metaSize := 0
		nameSize := 0
		valueSize := 0

		if isLocal {
			metaSize = 3
			vsize, ok := readUint16BE(data, valuesOffset)
			if !ok {
				return nil, wrapParseError(int64(valuesOffset), "xattr_leaf_local_value_size", ErrInvalidAttributeData)
			}
			valueSize = int(vsize)
			if valuesOffset+2 >= len(data) {
				return nil, wrapParseError(int64(valuesOffset+2), "xattr_leaf_local_name_size", ErrInvalidAttributeData)
			}
			nameSize = int(data[valuesOffset+2])
		} else {
			metaSize = 9
			if valuesOffset+9 > len(data) {
				return nil, wrapParseError(int64(valuesOffset), "xattr_leaf_remote_values", ErrInvalidAttributeData)
			}
			vsize, ok := readUint32BE(data, valuesOffset+4)
			if !ok {
				return nil, wrapParseError(int64(valuesOffset+4), "xattr_leaf_remote_value_size", ErrInvalidAttributeData)
			}
			valueSize = int(vsize)
			nameSize = int(data[valuesOffset+8])
		}

		if valuesOffset+metaSize+nameSize > len(data) {
			return nil, wrapParseError(int64(valuesOffset), "xattr_leaf_name_bounds", ErrInvalidAttributeData)
		}

		nameStart := valuesOffset + metaSize
		name := string(data[nameStart : nameStart+nameSize])
		prefix, namespace, err := xattrNamespaceFromFlags(flags)
		if err != nil {
			return nil, fmt.Errorf("xattr_leaf[%d]: %w", i, err)
		}

		var value []byte
		if isLocal {
			valueStart := nameStart + nameSize
			if valueStart+valueSize > len(data) {
				return nil, wrapParseError(int64(valueStart), "xattr_leaf_value_bounds", ErrInvalidAttributeData)
			}
			value = append([]byte(nil), data[valueStart:valueStart+valueSize]...)
		} else {
			valueBlockNumber, ok := readUint32BE(data, valuesOffset)
			if !ok {
				return nil, wrapParseError(int64(valuesOffset), "xattr_leaf_remote_value_block", ErrInvalidAttributeData)
			}
			remoteValue, err := v.readRemoteAttributeValue(inode, valueBlockNumber, uint32(valueSize))
			if err != nil {
				return nil, fmt.Errorf("xattr_leaf[%d]: %w", i, err)
			}
			value = remoteValue
		}

		out = append(out, ExtendedAttribute{
			Name:      prefix + name,
			Namespace: namespace,
			Value:     value,
			Flags:     flags,
		})
	}

	return out, nil
}

func (v *Volume) readRemoteAttributeValue(startInode *Inode, startBlock uint32, valueSize uint32) ([]byte, error) {
	if valueSize == 0 {
		return nil, nil
	}

	out := make([]byte, 0, valueSize)
	remaining := int(valueSize)
	currentBlock := startBlock

	for remaining > 0 {
		block, err := v.readAttributesLogicalBlock(startInode, currentBlock)
		if err != nil {
			return nil, err
		}

		segment := block
		if v.ioh.formatVersion == 5 && len(block) >= 48 && string(block[0:4]) == "XARM" {
			valueDataOffset, ok := readUint32BE(block, 4)
			if !ok {
				return nil, wrapParseError(4, "xattr_remote_value_offset", ErrInvalidAttributeData)
			}
			valueDataSize, ok := readUint32BE(block, 8)
			if !ok {
				return nil, wrapParseError(8, "xattr_remote_value_size", ErrInvalidAttributeData)
			}

			off := int(valueDataOffset)
			sz := int(valueDataSize)
			if off < 48 || off > len(block) || sz < 0 || off+sz > len(block) {
				return nil, wrapParseError(int64(off), "xattr_remote_value_bounds", ErrInvalidAttributeData)
			}
			segment = block[off : off+sz]
		}

		toCopy := len(segment)
		if toCopy > remaining {
			toCopy = remaining
		}
		if toCopy <= 0 {
			return nil, wrapParseError(0, "xattr_remote_value_segment", ErrInvalidAttributeData)
		}
		out = append(out, segment[:toCopy]...)
		remaining -= toCopy
		currentBlock++
	}

	return out, nil
}
