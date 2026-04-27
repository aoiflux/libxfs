package libxfs

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sync"
)

type ioHandle struct {
	formatVersion           uint8
	secondaryFeatureFlags   uint32
	blockSize               uint32
	allocationGroupSize     uint32
	inodeSize               uint16
	directoryBlockSize      uint32
	relativeBlockNumberBits uint8
	relativeInodeNumberBits uint8
}

// Volume is an XFS volume parser with concurrency-safe read APIs.
type Volume struct {
	reader       io.ReaderAt
	sourceCloser io.Closer

	sb      *Superblock
	ioh     ioHandle
	agInode []InodeInformation

	inodeCache   map[uint64]*Inode
	inodeCacheMu sync.RWMutex

	closed  bool
	closeMu sync.RWMutex
}

// Open parses an XFS volume from a random-access reader.
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

func (v *Volume) Close() error {
	v.closeMu.Lock()
	defer v.closeMu.Unlock()

	if v.closed {
		return ErrVolumeClosed
	}
	v.closed = true

	v.inodeCacheMu.Lock()
	v.inodeCache = nil
	v.inodeCacheMu.Unlock()

	if v.sourceCloser != nil {
		if err := v.sourceCloser.Close(); err != nil {
			return wrapVolumeError("close", err)
		}
		v.sourceCloser = nil
	}

	return nil
}

func (v *Volume) IsClosed() bool {
	v.closeMu.RLock()
	defer v.closeMu.RUnlock()
	return v.closed
}

func (v *Volume) Superblock() Superblock {
	if v.sb == nil {
		return Superblock{}
	}
	return *v.sb
}

func (v *Volume) GetRootInode() (*Inode, error) {
	if v.sb == nil {
		return nil, wrapVolumeError("root_inode", ErrInvalidSuperblock)
	}
	return v.OpenInode(v.sb.RootDirectoryInodeNumber)
}

func (v *Volume) OpenInode(inodeNumber uint64) (*Inode, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}
	if inodeNumber == 0 || inodeNumber > math.MaxUint32 {
		return nil, ErrInvalidInodeNumber
	}

	v.inodeCacheMu.RLock()
	if inode, ok := v.inodeCache[inodeNumber]; ok {
		v.inodeCacheMu.RUnlock()
		return inode, nil
	}
	v.inodeCacheMu.RUnlock()

	bits := v.ioh.relativeInodeNumberBits
	if bits == 0 || bits >= 32 {
		return nil, wrapVolumeError("open_inode", wrapParseError(0, "relative_inode_bits", ErrInvalidSuperblock))
	}

	allocationGroupIndex := int(inodeNumber >> bits)
	relativeInodeNumber := inodeNumber & ((uint64(1) << bits) - 1)

	if allocationGroupIndex < 0 || allocationGroupIndex >= len(v.agInode) {
		return nil, fmt.Errorf("%w: allocation group index out of bounds", ErrInvalidInodeNumber)
	}

	found, err := v.hasRelativeInodeInBtree(allocationGroupIndex, relativeInodeNumber)
	if err != nil {
		return nil, fmt.Errorf("inode btree lookup failed: %w", err)
	}
	if !found {
		return nil, ErrInodeNotFound
	}

	agBlockNumber := uint64(allocationGroupIndex) * uint64(v.ioh.allocationGroupSize)
	offset := int64(agBlockNumber)*int64(v.ioh.blockSize) + int64(relativeInodeNumber)*int64(v.ioh.inodeSize)

	buf := make([]byte, v.ioh.inodeSize)
	if err := readAtFull(v.reader, buf, offset); err != nil {
		return nil, wrapIOError("read", offset, len(buf), err)
	}

	inode, err := parseInode(buf, v.ioh.blockSize)
	if err != nil {
		return nil, err
	}
	if inode.ForkType == ForkTypeBtree {
		if err := v.populateExtentBtreeExtents(inode); err != nil {
			return nil, err
		}
	}
	if err := v.populateAttributesFork(inode); err != nil {
		return nil, err
	}

	v.inodeCacheMu.Lock()
	v.inodeCache[inodeNumber] = inode
	v.inodeCacheMu.Unlock()

	return inode, nil
}

// ReadInodeData reads file data from an inode at offset.
// It supports inline data and extent-list backed regular files.
func (v *Volume) ReadInodeData(inodeNumber uint64, p []byte, off int64) (int, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return 0, err
	}
	return v.readInodeData(inode, p, off)
}

// ReadInodeAttributeForkData reads the inode attributes fork payload.
// For inline attributes, this returns the inline bytes; for extent/btree forks,
// it reconstructs data from mapped extents up to the attributes fork size.
func (v *Volume) ReadInodeAttributeForkData(inodeNumber uint64) ([]byte, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return nil, err
	}
	if inode.AttributesForkSize == 0 {
		return nil, nil
	}

	if inode.AttributesForkType == ForkTypeInlineData {
		return append([]byte(nil), inode.InlineAttributesData...), nil
	}
	if inode.AttributesForkType != ForkTypeExtents && inode.AttributesForkType != ForkTypeBtree {
		return nil, fmt.Errorf("unsupported attributes fork type: %d", inode.AttributesForkType)
	}

	out := make([]byte, inode.AttributesForkSize)
	n, err := v.readFromExtents(inode.AttributesExtents, uint64(inode.AttributesForkSize), out, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n < len(out) {
		for i := n; i < len(out); i++ {
			out[i] = 0
		}
	}
	return out, nil
}

// ListInodeExtendedAttributes lists decoded inode extended attributes.
//
// For block-based attribute trees, returned entries preserve traversal order.
// If duplicate fully-qualified names are present, duplicates are preserved in
// the returned slice (no deduplication is performed).
func (v *Volume) ListInodeExtendedAttributes(inodeNumber uint64) ([]ExtendedAttribute, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return nil, err
	}

	attrData, err := v.ReadInodeAttributeForkData(inodeNumber)
	if err != nil {
		return nil, err
	}
	if len(attrData) == 0 {
		return nil, nil
	}

	attrs, err := parseShortFormAttributes(attrData)
	if err == nil {
		return attrs, nil
	}

	if inode.AttributesForkType == ForkTypeExtents || inode.AttributesForkType == ForkTypeBtree {
		attrs, blockErr := v.parseAttributesFromBlocks(inode)
		if blockErr == nil {
			return attrs, nil
		}
		return nil, blockErr
	}

	return nil, fmt.Errorf("%w: only short-form xattrs are currently supported", ErrUnsupportedXattrFormat)
}

func (v *Volume) readInodeData(inode *Inode, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, wrapParseError(off, "offset", ErrInvalidInode)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if uint64(off) >= inode.Size {
		return 0, io.EOF
	}

	remaining := int(inode.Size - uint64(off))
	if remaining > len(p) {
		remaining = len(p)
	}

	if inode.ForkType == ForkTypeInlineData {
		n := copy(p[:remaining], inode.InlineData[off:int(off)+remaining])
		if n < len(p) {
			return n, io.EOF
		}
		return n, nil
	}

	if inode.ForkType != ForkTypeExtents && inode.ForkType != ForkTypeBtree {
		return 0, fmt.Errorf("unsupported inode fork type: %d", inode.ForkType)
	}
	return v.readFromExtents(inode.DataExtents, inode.Size, p, off)
}

func (v *Volume) readFromExtents(extents []Extent, totalSize uint64, p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, wrapParseError(off, "offset", ErrInvalidInode)
	}
	if len(p) == 0 {
		return 0, nil
	}
	if uint64(off) >= totalSize {
		return 0, io.EOF
	}
	if v.ioh.blockSize == 0 {
		return 0, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}

	remaining := int(totalSize - uint64(off))
	if remaining > len(p) {
		remaining = len(p)
	}

	written := 0
	for written < remaining {
		pos := uint64(off) + uint64(written)
		logicalBlock := pos / uint64(v.ioh.blockSize)
		inBlockOffset := pos % uint64(v.ioh.blockSize)

		extent, nextLogical := findExtentForBlock(extents, logicalBlock)

		if extent == nil {
			// Treat unlisted range as sparse/hole.
			holeBytes := uint64(remaining - written)
			if nextLogical > logicalBlock {
				gapBlocks := nextLogical - logicalBlock
				gapBytes := gapBlocks*uint64(v.ioh.blockSize) - inBlockOffset
				if gapBytes < holeBytes {
					holeBytes = gapBytes
				}
			}
			for i := 0; i < int(holeBytes); i++ {
				p[written+i] = 0
			}
			written += int(holeBytes)
			continue
		}

		extentBlockIndex := logicalBlock - extent.LogicalBlockNumber
		extentRemainingBytes := (uint64(extent.NumberOfBlocks)-extentBlockIndex)*uint64(v.ioh.blockSize) - inBlockOffset
		toRead := uint64(remaining - written)
		if extentRemainingBytes < toRead {
			toRead = extentRemainingBytes
		}

		if (extent.RangeFlags & ExtentFlagSparse) != 0 {
			for i := 0; i < int(toRead); i++ {
				p[written+i] = 0
			}
			written += int(toRead)
			continue
		}

		physicalBlock := extent.PhysicalBlockNumber + extentBlockIndex
		diskOff := int64(physicalBlock*uint64(v.ioh.blockSize) + inBlockOffset)
		if err := readAtFull(v.reader, p[written:written+int(toRead)], diskOff); err != nil {
			return written, wrapIOError("read", diskOff, int(toRead), err)
		}
		written += int(toRead)
	}

	if written < len(p) {
		return written, io.EOF
	}
	return written, nil
}

func findExtentForBlock(extents []Extent, logicalBlock uint64) (*Extent, uint64) {
	var nextLogical uint64 = math.MaxUint64
	for i := range extents {
		extent := &extents[i]
		start := extent.LogicalBlockNumber
		end := start + uint64(extent.NumberOfBlocks)
		if logicalBlock >= start && logicalBlock < end {
			return extent, nextLogical
		}
		if start > logicalBlock && start < nextLogical {
			nextLogical = start
		}
	}
	return nil, nextLogical
}

func (v *Volume) populateExtentBtreeExtents(inode *Inode) error {
	if inode == nil {
		return wrapParseError(0, "inode", ErrInvalidInode)
	}
	if v.ioh.blockSize == 0 {
		return wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}
	if inode.DataForkSize < 4 {
		return wrapParseError(int64(inode.DataForkOffset), "extent_btree_root", ErrInvalidInode)
	}

	start := int(inode.DataForkOffset)
	end := start + int(inode.DataForkSize)
	if start < 0 || end > len(inode.Raw) || start > end {
		return wrapParseError(int64(start), "data_fork", ErrInvalidInode)
	}

	numberOfBlocks := inode.Size / uint64(v.ioh.blockSize)
	if inode.Size%uint64(v.ioh.blockSize) != 0 {
		numberOfBlocks++
	}

	addSparseExtents := !inode.IsDirectory()
	extents, err := v.parseExtentsFromBtreeRoot(inode.Raw[start:end], numberOfBlocks, addSparseExtents)
	if err != nil {
		return err
	}
	inode.DataExtents = extents
	return nil
}

func (v *Volume) populateAttributesFork(inode *Inode) error {
	if inode == nil {
		return wrapParseError(0, "inode", ErrInvalidInode)
	}
	if inode.AttributesForkSize == 0 {
		return nil
	}

	start := int(inode.AttributesForkOffset)
	end := start + int(inode.AttributesForkSize)
	if start < 0 || end > len(inode.Raw) || start > end {
		return wrapParseError(int64(start), "attributes_fork", ErrInvalidInode)
	}
	data := inode.Raw[start:end]

	numberOfBlocks := uint64(inode.AttributesForkSize) / uint64(v.ioh.blockSize)
	if uint64(inode.AttributesForkSize)%uint64(v.ioh.blockSize) != 0 {
		numberOfBlocks++
	}

	switch inode.AttributesForkType {
	case ForkTypeInlineData:
		inode.InlineAttributesData = append([]byte(nil), data...)
	case ForkTypeExtents:
		if inode.NumberOfAttributesExtent == 0 {
			return nil
		}
		extents, err := parseExtentList(numberOfBlocks, uint32(inode.NumberOfAttributesExtent), data, false)
		if err != nil {
			return err
		}
		inode.AttributesExtents = extents
	case ForkTypeBtree:
		extents, err := v.parseExtentsFromBtreeRoot(data, numberOfBlocks, false)
		if err != nil {
			return err
		}
		inode.AttributesExtents = extents
	}
	return nil
}

func (v *Volume) parseExtentsFromBtreeRoot(data []byte, numberOfBlocks uint64, addSparseExtents bool) ([]Extent, error) {
	if len(data) < 4 {
		return nil, wrapParseError(0, "extent_btree_root", ErrInvalidInode)
	}

	level, ok := readUint16BE(data, 0)
	if !ok {
		return nil, wrapParseError(0, "extent_btree_level", ErrInvalidInode)
	}
	numberOfRecords, ok := readUint16BE(data, 2)
	if !ok {
		return nil, wrapParseError(2, "extent_btree_number_of_records", ErrInvalidInode)
	}
	if level == 0 {
		return nil, wrapParseError(0, "extent_btree_level", ErrInvalidInode)
	}

	extents, err := v.getExtentsFromExtentBtreeBranchNode(numberOfRecords, data[4:], addSparseExtents, int(level), 0)
	if err != nil {
		return nil, err
	}

	if addSparseExtents {
		logicalBlockNumber := uint64(0)
		if len(extents) > 0 {
			last := extents[len(extents)-1]
			logicalBlockNumber = last.LogicalBlockNumber + uint64(last.NumberOfBlocks)
		}
		if logicalBlockNumber < numberOfBlocks {
			if len(extents) == 0 || (extents[len(extents)-1].RangeFlags&ExtentFlagSparse) == 0 {
				extents = append(extents, Extent{
					LogicalBlockNumber: logicalBlockNumber,
					RangeFlags:         ExtentFlagSparse,
				})
			}
			extents[len(extents)-1].NumberOfBlocks += uint32(numberOfBlocks - logicalBlockNumber)
		}
	}

	return extents, nil
}

func (v *Volume) getExtentsFromExtentBtreeBranchNode(numberOfRecords uint16, recordsData []byte, addSparseExtents bool, maximumDepth int, recursionDepth int) ([]Extent, error) {
	if recursionDepth < 0 || recursionDepth > maxBtreeRecursionDepth {
		return nil, wrapParseError(0, "extent_btree_recursion_depth", ErrInvalidInode)
	}

	numberOfKeyValuePairs := len(recordsData) / 16
	if int(numberOfRecords) > numberOfKeyValuePairs {
		return nil, wrapParseError(0, "extent_btree_branch_records", ErrInvalidInode)
	}

	valuesOffset := numberOfKeyValuePairs * 8
	result := make([]Extent, 0)

	for i := 0; i < int(numberOfRecords); i++ {
		subBlock, ok := readUint64BE(recordsData, valuesOffset+i*8)
		if !ok {
			return nil, wrapParseError(int64(valuesOffset+i*8), "extent_btree_branch_value", ErrInvalidInode)
		}

		extents, err := v.getExtentsFromExtentBtreeNode(subBlock, addSparseExtents, maximumDepth, recursionDepth+1)
		if err != nil {
			return nil, err
		}
		result = append(result, extents...)
	}

	return result, nil
}

func (v *Volume) getExtentsFromExtentBtreeNode(blockNumber uint64, addSparseExtents bool, maximumDepth int, recursionDepth int) ([]Extent, error) {
	if v.ioh.allocationGroupSize == 0 {
		return nil, wrapParseError(0, "allocation_group_size", ErrInvalidSuperblock)
	}
	if v.ioh.blockSize == 0 {
		return nil, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}
	if recursionDepth < 0 || recursionDepth > maxBtreeRecursionDepth {
		return nil, wrapParseError(0, "extent_btree_recursion_depth", ErrInvalidInode)
	}

	allocationGroupIndex := blockNumber >> v.ioh.relativeBlockNumberBits
	relativeBlockNumber := blockNumber & ((uint64(1) << v.ioh.relativeBlockNumberBits) - 1)

	offsetBlocks := allocationGroupIndex*uint64(v.ioh.allocationGroupSize) + relativeBlockNumber
	if offsetBlocks > uint64(math.MaxInt64)/uint64(v.ioh.blockSize) {
		return nil, wrapParseError(0, "extent_btree_block_number", ErrInvalidInode)
	}
	offset := int64(offsetBlocks * uint64(v.ioh.blockSize))

	blockData := make([]byte, v.ioh.blockSize)
	if err := readAtFull(v.reader, blockData, offset); err != nil {
		return nil, wrapIOError("read", offset, len(blockData), err)
	}

	header, headerSize, err := parseBtreeBlockHeader(blockData, v.ioh.formatVersion, 8)
	if err != nil {
		return nil, err
	}
	recordsData := blockData[headerSize:]

	if v.ioh.formatVersion == 5 {
		if header.Signature != "BMA3" {
			return nil, wrapParseError(offset, "extent_btree_signature", ErrInvalidInode)
		}
	} else {
		if header.Signature != "BMAP" {
			return nil, wrapParseError(offset, "extent_btree_signature", ErrInvalidInode)
		}
	}

	if int(header.Level) > maximumDepth {
		return nil, wrapParseError(offset, "extent_btree_level", ErrInvalidInode)
	}

	if header.Level == 0 {
		return parseExtentList(0, uint32(header.NumberOfRecords), recordsData, addSparseExtents)
	}

	return v.getExtentsFromExtentBtreeBranchNode(header.NumberOfRecords, recordsData, addSparseExtents, maximumDepth, recursionDepth)
}

func (v *Volume) hasRelativeInodeInBtree(allocationGroupIndex int, relativeInodeNumber uint64) (bool, error) {
	if allocationGroupIndex < 0 || allocationGroupIndex >= len(v.agInode) {
		return false, fmt.Errorf("%w: allocation group index out of bounds", ErrInvalidInodeNumber)
	}
	agInfo := v.agInode[allocationGroupIndex]
	agBlockNumber := uint64(allocationGroupIndex) * uint64(v.ioh.allocationGroupSize)

	return v.getRelativeInodeFromNode(
		agBlockNumber,
		agInfo.InodeBtreeRootBlock,
		relativeInodeNumber,
		0,
	)
}

func (v *Volume) getRelativeInodeFromNode(allocationGroupBlockNumber uint64, relativeBlockNumber uint32, relativeInodeNumber uint64, recursionDepth int) (bool, error) {
	if recursionDepth < 0 || recursionDepth > maxBtreeRecursionDepth {
		return false, wrapParseError(0, "btree_recursion_depth", ErrInvalidInode)
	}
	if v.ioh.blockSize == 0 {
		return false, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}

	totalBlocks := allocationGroupBlockNumber + uint64(relativeBlockNumber)
	if totalBlocks < allocationGroupBlockNumber {
		return false, wrapParseError(0, "relative_block_number", ErrInvalidInode)
	}
	if totalBlocks > uint64(math.MaxInt64)/uint64(v.ioh.blockSize) {
		return false, wrapParseError(0, "relative_block_number", ErrInvalidInode)
	}
	offset := int64(totalBlocks * uint64(v.ioh.blockSize))

	blockData := make([]byte, v.ioh.blockSize)
	if err := readAtFull(v.reader, blockData, offset); err != nil {
		return false, wrapIOError("read", offset, len(blockData), err)
	}

	header, headerSize, err := parseBtreeBlockHeader(blockData, v.ioh.formatVersion, 4)
	if err != nil {
		return false, err
	}
	recordsData := blockData[headerSize:]

	if v.ioh.formatVersion == 5 {
		if header.Signature != "IAB3" {
			return false, wrapParseError(offset, "btree_signature", ErrInvalidInode)
		}
	} else {
		if header.Signature != "IABT" {
			return false, wrapParseError(offset, "btree_signature", ErrInvalidInode)
		}
	}

	if header.Level == 0 {
		return inodeFoundInLeafRecords(recordsData, header.NumberOfRecords, relativeInodeNumber, offset)
	}

	subBlockNumber, ok, err := subBlockForRelativeInode(recordsData, header.NumberOfRecords, relativeInodeNumber, offset)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}

	return v.getRelativeInodeFromNode(
		allocationGroupBlockNumber,
		subBlockNumber,
		relativeInodeNumber,
		recursionDepth+1,
	)
}

type btreeBlockHeader struct {
	Signature       string
	Level           uint16
	NumberOfRecords uint16
}

func parseBtreeBlockHeader(data []byte, formatVersion uint8, blockNumberDataSize int) (*btreeBlockHeader, int, error) {
	if blockNumberDataSize != 4 && blockNumberDataSize != 8 {
		return nil, 0, wrapParseError(0, "block_number_data_size", ErrInvalidInode)
	}

	headerSize := 0
	if formatVersion == 5 {
		if blockNumberDataSize == 8 {
			headerSize = 72
		} else {
			headerSize = 56
		}
	} else {
		if blockNumberDataSize == 8 {
			headerSize = 24
		} else {
			headerSize = 16
		}
	}
	if len(data) < headerSize {
		return nil, 0, wrapParseError(0, "btree_header", ErrInvalidInode)
	}

	level, ok := readUint16BE(data, 4)
	if !ok {
		return nil, 0, wrapParseError(4, "btree_level", ErrInvalidInode)
	}
	numberOfRecords, ok := readUint16BE(data, 6)
	if !ok {
		return nil, 0, wrapParseError(6, "btree_number_of_records", ErrInvalidInode)
	}

	return &btreeBlockHeader{
		Signature:       string(data[0:4]),
		Level:           level,
		NumberOfRecords: numberOfRecords,
	}, headerSize, nil
}

func inodeFoundInLeafRecords(recordsData []byte, numberOfRecords uint16, relativeInodeNumber uint64, offset int64) (bool, error) {
	if int(numberOfRecords) > len(recordsData)/16 {
		return false, wrapParseError(offset, "leaf_number_of_records", ErrInvalidInode)
	}
	for i := 0; i < int(numberOfRecords); i++ {
		recordOffset := i * 16
		inodeNumber, ok := readUint32BE(recordsData, recordOffset)
		if !ok {
			return false, wrapParseError(offset+int64(recordOffset), "leaf_inode_number", ErrInvalidInode)
		}
		numberOfUnusedInodes, ok := readUint32BE(recordsData, recordOffset+4)
		if !ok {
			return false, wrapParseError(offset+int64(recordOffset+4), "leaf_unused_inode_count", ErrInvalidInode)
		}
		chunkAllocationBitmap, ok := readUint64BE(recordsData, recordOffset+8)
		if !ok {
			return false, wrapParseError(offset+int64(recordOffset+8), "leaf_chunk_bitmap", ErrInvalidInode)
		}
		if relativeInodeNumber >= uint64(inodeNumber) && relativeInodeNumber < uint64(inodeNumber)+64 {
			if !inodeAllocatedInChunk(relativeInodeNumber, uint64(inodeNumber), numberOfUnusedInodes, chunkAllocationBitmap) {
				return false, nil
			}
			return true, nil
		}
	}
	return false, nil
}

func inodeAllocatedInChunk(relativeInodeNumber uint64, chunkBaseInode uint64, numberOfUnusedInodes uint32, chunkAllocationBitmap uint64) bool {
	if numberOfUnusedInodes == 0 {
		return true
	}
	if relativeInodeNumber < chunkBaseInode || relativeInodeNumber >= chunkBaseInode+64 {
		return false
	}

	relativeIndex := relativeInodeNumber - chunkBaseInode

	// In XFS inobt records, bitmap bit set indicates inode is unused.
	isUnused := (chunkAllocationBitmap & (uint64(1) << relativeIndex)) != 0
	return !isUnused
}

func subBlockForRelativeInode(recordsData []byte, numberOfRecords uint16, relativeInodeNumber uint64, offset int64) (uint32, bool, error) {
	numberOfKeyValuePairs := len(recordsData) / 8
	if int(numberOfRecords) > numberOfKeyValuePairs {
		return 0, false, wrapParseError(offset, "branch_number_of_records", ErrInvalidInode)
	}

	recordsDataOffset := 0
	recordIndex := 0
	for ; recordIndex < int(numberOfRecords); recordIndex++ {
		relativeKeyInodeNumber, ok := readUint32BE(recordsData, recordsDataOffset)
		if !ok {
			return 0, false, wrapParseError(offset+int64(recordsDataOffset), "branch_key_inode", ErrInvalidInode)
		}
		recordsDataOffset += 4

		if relativeInodeNumber < uint64(relativeKeyInodeNumber) {
			break
		}
	}

	if recordIndex > 0 && recordIndex <= int(numberOfRecords) {
		recordsDataOffset = (numberOfKeyValuePairs + recordIndex - 1) * 4
		subBlockNumber, ok := readUint32BE(recordsData, recordsDataOffset)
		if !ok {
			return 0, false, wrapParseError(offset+int64(recordsDataOffset), "branch_sub_block", ErrInvalidInode)
		}
		return subBlockNumber, true, nil
	}
	return 0, false, nil
}

func (v *Volume) parseAllocationGroups() error {
	sbData := make([]byte, superblockSize)
	if err := readAtFull(v.reader, sbData, 0); err != nil {
		return wrapIOError("read", 0, superblockSize, err)
	}

	sb, err := parseSuperblock(sbData)
	if err != nil {
		return err
	}
	v.sb = sb
	v.ioh = ioHandle{
		formatVersion:           sb.FormatVersion,
		secondaryFeatureFlags:   sb.SecondaryFeatureFlags,
		blockSize:               sb.BlockSize,
		allocationGroupSize:     sb.AllocationGroupSize,
		inodeSize:               sb.InodeSize,
		directoryBlockSize:      sb.DirectoryBlockSize,
		relativeBlockNumberBits: sb.RelativeBlockNumberBits,
		relativeInodeNumberBits: sb.RelativeInodeNumberBits,
	}

	v.agInode = make([]InodeInformation, 0, sb.NumberOfAllocationGroups)
	for ag := uint32(0); ag < sb.NumberOfAllocationGroups; ag++ {
		superblockOffset := int64(ag) * int64(sb.AllocationGroupSize) * int64(sb.BlockSize)
		if err := readAtFull(v.reader, sbData, superblockOffset); err != nil {
			return wrapIOError("read", superblockOffset, superblockSize, err)
		}

		agSB, err := parseSuperblock(sbData)
		if err != nil {
			return fmt.Errorf("allocation-group=%d superblock: %w", ag, err)
		}
		if agSB.BlockSize != sb.BlockSize || agSB.InodeSize != sb.InodeSize {
			return fmt.Errorf("allocation-group=%d: inconsistent geometry", ag)
		}

		inodeInfoOffset := superblockOffset + int64(2*sb.SectorSize)
		agiData := make([]byte, inodeInformationLen)
		if err := readAtFull(v.reader, agiData, inodeInfoOffset); err != nil {
			return wrapIOError("read", inodeInfoOffset, inodeInformationLen, err)
		}

		agi, err := parseInodeInformation(agiData, sb.FormatVersion)
		if err != nil {
			return fmt.Errorf("allocation-group=%d inode-info: %w", ag, err)
		}
		v.agInode = append(v.agInode, *agi)
	}
	return nil
}

func parseSuperblock(data []byte) (*Superblock, error) {
	if len(data) < superblockSize {
		return nil, wrapParseError(0, "superblock", ErrInvalidSuperblock)
	}
	if !bytes.Equal(data[0:4], []byte(xfsSuperblockMagic)) {
		return nil, wrapParseError(0, "signature", ErrInvalidSuperblock)
	}

	blockSize, _ := readUint32BE(data, 4)
	numberOfBlocks, _ := readUint64BE(data, 8)
	journalBlockNumber, _ := readUint64BE(data, 48)
	rootInode, _ := readUint64BE(data, 56)
	allocationGroupSize, _ := readUint32BE(data, 84)
	numberOfAllocationGroups, _ := readUint32BE(data, 88)
	versionAndFlags, _ := readUint16BE(data, 100)
	sectorSize, _ := readUint16BE(data, 102)
	inodeSize, _ := readUint16BE(data, 104)
	numberOfInodesPerBlock, _ := readUint16BE(data, 106)
	secondaryFeatureFlags, _ := readUint32BE(data, 204)

	var volumeLabel [12]byte
	copy(volumeLabel[:], data[108:120])

	formatVersion := uint8(versionAndFlags & 0x000f)
	featureFlags := versionAndFlags & 0xfff0

	if formatVersion != 4 && formatVersion != 5 {
		return nil, wrapParseError(100, "format_version", ErrInvalidSuperblock)
	}

	supportedFeatureFlags := uint16(0x0010 | 0x0020 | 0x0080 | 0x0400 | 0x0800 | 0x1000 | 0x2000 | 0x4000 | 0x8000)
	if featureFlags&^supportedFeatureFlags != 0 {
		return nil, wrapParseError(100, "feature_flags", ErrUnsupportedFeatureFlag)
	}

	if blockSize < minBlockSize || blockSize > maxBlockSize {
		return nil, wrapParseError(4, "block_size", ErrInvalidSuperblock)
	}
	if sectorSize != 512 && sectorSize != 1024 && sectorSize != 2048 && sectorSize != 4096 && sectorSize != 8192 && sectorSize != 16384 {
		return nil, wrapParseError(102, "sector_size", ErrInvalidSuperblock)
	}
	if inodeSize < minInodeSize || inodeSize > maxInodeSize {
		return nil, wrapParseError(104, "inode_size", ErrInvalidSuperblock)
	}

	dirLog2 := uint32(data[192])
	directoryBlockSize := blockSize
	if dirLog2 != 0 {
		if dirLog2 >= 32 {
			return nil, wrapParseError(192, "directory_block_size_log2", ErrInvalidSuperblock)
		}
		directoryBlockSize = uint32(1) << dirLog2
		if directoryBlockSize > math.MaxUint32/blockSize {
			return nil, wrapParseError(192, "directory_block_size_log2", ErrInvalidSuperblock)
		}
		directoryBlockSize *= blockSize
	}

	if allocationGroupSize < 5 || allocationGroupSize > math.MaxInt32 {
		return nil, wrapParseError(84, "allocation_group_size", ErrInvalidSuperblock)
	}
	allocationGroupSizeLog2 := data[124]
	if allocationGroupSizeLog2 == 0 || allocationGroupSizeLog2 > 31 {
		return nil, wrapParseError(124, "allocation_group_size_log2", ErrInvalidSuperblock)
	}

	numberOfInodesPerBlockLog2 := data[123]
	relativeBlockBits := allocationGroupSizeLog2
	if numberOfInodesPerBlockLog2 == 0 || int(numberOfInodesPerBlockLog2) > (32-int(relativeBlockBits)) {
		return nil, wrapParseError(123, "number_of_inodes_per_block_log2", ErrInvalidSuperblock)
	}
	relativeInodeBits := relativeBlockBits + numberOfInodesPerBlockLog2
	if relativeInodeBits == 0 || relativeInodeBits >= 32 {
		return nil, wrapParseError(123, "relative_inode_number_bits", ErrInvalidSuperblock)
	}
	if uint16(1)<<numberOfInodesPerBlockLog2 != numberOfInodesPerBlock {
		return nil, wrapParseError(106, "number_of_inodes_per_block", ErrInvalidSuperblock)
	}

	return &Superblock{
		BlockSize:                blockSize,
		NumberOfBlocks:           numberOfBlocks,
		JournalBlockNumber:       journalBlockNumber,
		RootDirectoryInodeNumber: rootInode,
		AllocationGroupSize:      allocationGroupSize,
		NumberOfAllocationGroups: numberOfAllocationGroups,
		FormatVersion:            formatVersion,
		FeatureFlags:             featureFlags,
		SectorSize:               sectorSize,
		InodeSize:                inodeSize,
		DirectoryBlockSize:       directoryBlockSize,
		VolumeLabel:              volumeLabel,
		SecondaryFeatureFlags:    secondaryFeatureFlags,
		RelativeBlockNumberBits:  relativeBlockBits,
		RelativeInodeNumberBits:  relativeInodeBits,
	}, nil
}

func parseInodeInformation(data []byte, formatVersion uint8) (*InodeInformation, error) {
	required := 0
	if formatVersion >= 5 {
		required = 512
	} else {
		required = 296
	}
	if len(data) < required {
		return nil, wrapParseError(0, "inode_information", ErrInvalidInodeInfo)
	}
	if !bytes.Equal(data[0:4], []byte(xfsAGIMagic)) {
		return nil, wrapParseError(0, "signature", ErrInvalidInodeInfo)
	}

	fv, _ := readUint32BE(data, 4)
	root, _ := readUint32BE(data, 20)
	depth, _ := readUint32BE(data, 24)
	lastChunk, _ := readUint32BE(data, 32)

	return &InodeInformation{
		FormatVersion:       fv,
		InodeBtreeRootBlock: root,
		InodeBtreeDepth:     depth,
		LastAllocatedChunk:  lastChunk,
	}, nil
}

func parseInode(data []byte, blockSize uint32) (*Inode, error) {
	if len(data) < minInodeSize {
		return nil, wrapParseError(0, "inode", ErrInvalidInode)
	}
	if !bytes.Equal(data[0:2], []byte(xfsInodeMagic)) {
		return nil, wrapParseError(0, "signature", ErrInvalidInode)
	}

	formatVersion := data[4]
	inodeHeaderSize := 100
	if formatVersion == 3 {
		inodeHeaderSize = 176
	}
	if len(data) < inodeHeaderSize {
		return nil, wrapParseError(0, "inode_header", ErrInvalidInode)
	}
	if formatVersion != 1 && formatVersion != 2 && formatVersion != 3 {
		return nil, wrapParseError(4, "format_version", ErrInvalidInode)
	}

	fileMode, _ := readUint16BE(data, 2)
	forkType := data[5]
	ownerID, _ := readUint32BE(data, 8)
	groupID, _ := readUint32BE(data, 12)

	var numberOfLinks uint32
	if formatVersion == 1 {
		v, _ := readUint16BE(data, 6)
		numberOfLinks = uint32(v)
	} else {
		numberOfLinks, _ = readUint32BE(data, 16)
	}

	atSec, _ := readUint32BE(data, 32)
	atNs, _ := readUint32BE(data, 36)
	mtSec, _ := readUint32BE(data, 40)
	mtNs, _ := readUint32BE(data, 44)
	ctSec, _ := readUint32BE(data, 48)
	ctNs, _ := readUint32BE(data, 52)
	size, _ := readUint64BE(data, 56)
	nDataExtents, _ := readUint32BE(data, 76)
	nAttrExtents, _ := readUint16BE(data, 80)
	attrForkOffset := uint16(data[82]) * 8
	attrForkType := data[83]

	inode := &Inode{
		FormatVersion:            formatVersion,
		FileMode:                 fileMode,
		ForkType:                 forkType,
		OwnerID:                  ownerID,
		GroupID:                  groupID,
		NumberOfLinks:            numberOfLinks,
		AccessTimeNS:             signedSecondsWithNanos(atSec, atNs),
		ModificationTimeNS:       signedSecondsWithNanos(mtSec, mtNs),
		InodeChangeTimeNS:        signedSecondsWithNanos(ctSec, ctNs),
		Size:                     size,
		NumberOfDataExtents:      nDataExtents,
		NumberOfAttributesExtent: nAttrExtents,
		AttributesForkType:       attrForkType,
		Raw:                      append([]byte(nil), data...),
	}

	if formatVersion == 3 {
		createSec, _ := readUint32BE(data, 144)
		createNs, _ := readUint32BE(data, 148)
		inode.CreationTimeNS = signedSecondsWithNanos(createSec, createNs)
	}

	dataForkSize := len(data) - inodeHeaderSize
	if attrForkOffset > 0 {
		if int(attrForkOffset) >= dataForkSize {
			return nil, wrapParseError(82, "attributes_fork_offset", ErrInvalidInode)
		}
		dataForkSize = int(attrForkOffset)
		inode.AttributesForkOffset = uint16(inodeHeaderSize) + attrForkOffset
		inode.AttributesForkSize = uint16(len(data) - int(inode.AttributesForkOffset))
	}
	inode.DataForkOffset = uint16(inodeHeaderSize)
	inode.DataForkSize = uint16(dataForkSize)

	if inode.ForkType == ForkTypeInlineData && inode.Size > uint64(inode.DataForkSize) {
		return nil, wrapParseError(int64(inode.DataForkOffset), "inline_data_size", ErrInvalidInode)
	}
	if inode.ForkType == ForkTypeDevice {
		if int(inode.DataForkOffset)+4 > len(data) {
			return nil, wrapParseError(int64(inode.DataForkOffset), "device_identifier", ErrInvalidInode)
		}
		inode.DeviceIdentifier, _ = readUint32BE(data, int(inode.DataForkOffset))
	}
	if inode.ForkType == ForkTypeInlineData {
		start := int(inode.DataForkOffset)
		end := start + int(inode.Size)
		if end > len(data) {
			return nil, wrapParseError(int64(start), "inline_data", ErrInvalidInode)
		}
		inode.InlineData = append([]byte(nil), data[start:end]...)
	}
	if inode.ForkType == ForkTypeExtents {
		if blockSize == 0 {
			return nil, wrapParseError(0, "block_size", ErrInvalidSuperblock)
		}
		start := int(inode.DataForkOffset)
		end := start + int(inode.DataForkSize)
		if start < 0 || end > len(data) || start > end {
			return nil, wrapParseError(int64(start), "data_fork", ErrInvalidInode)
		}

		numberOfBlocks := inode.Size / uint64(blockSize)
		if inode.Size%uint64(blockSize) != 0 {
			numberOfBlocks++
		}
		addSparseExtents := !inode.IsDirectory()
		numberOfExtents := inode.NumberOfDataExtents
		if numberOfExtents == 0 {
			numberOfExtents = inferExtentCount(data[start:end])
		}
		if numberOfExtents == 0 {
			return inode, nil
		}

		extents, err := parseExtentList(numberOfBlocks, numberOfExtents, data[start:end], addSparseExtents)
		if err != nil {
			return nil, err
		}
		inode.DataExtents = extents
	}

	return inode, nil
}

func readAtFull(reader io.ReaderAt, buf []byte, off int64) error {
	read := 0
	for read < len(buf) {
		n, err := reader.ReadAt(buf[read:], off+int64(read))
		if n > 0 {
			read += n
		}
		if err != nil {
			if err == io.EOF && read == len(buf) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrUnexpectedEOF
		}
	}
	return nil
}
