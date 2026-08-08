package libxfs

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sort"
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
	bigTime                 bool
	largeExtentCounts       bool
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

	// closed and the backing reader are guarded by stateMu. See
	// volume_access.go for the concurrency model.
	closed  bool
	stateMu sync.RWMutex
}

// inodeFeatures returns the superblock feature bits that change inode layout.
func (v *Volume) inodeFeatures() inodeFeatures {
	return inodeFeatures{
		bigTime:           v.ioh.bigTime,
		largeExtentCounts: v.ioh.largeExtentCounts,
	}
}

// Close releases the volume.
//
// It waits for in-flight reads to finish before releasing the backing reader,
// so it is safe to call concurrently with reads. Subsequent operations return
// ErrVolumeClosed. Closing an already closed volume returns ErrVolumeClosed.
func (v *Volume) Close() error {
	v.stateMu.Lock()
	defer v.stateMu.Unlock()

	if v.closed {
		return ErrVolumeClosed
	}
	v.closed = true

	v.clearInodeCache()

	if v.sourceCloser != nil {
		if err := v.sourceCloser.Close(); err != nil {
			return wrapVolumeError("close", err)
		}
		v.sourceCloser = nil
	}

	return nil
}

// IsClosed reports whether the volume has been closed.
//
// It is a point-in-time answer: on a volume shared with a goroutine that may
// call Close, prefer acting on ErrVolumeClosed from the operation itself.
func (v *Volume) IsClosed() bool {
	v.stateMu.RLock()
	defer v.stateMu.RUnlock()
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

	if cached := v.lookupCachedInode(inodeNumber); cached != nil {
		return shallowCopyInode(cached), nil
	}

	bits := v.ioh.relativeInodeNumberBits
	if bits == 0 || bits >= maxRelativeInodeBits {
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
	if err := v.readAt(buf, offset); err != nil {
		return nil, wrapIOError("read", offset, len(buf), err)
	}

	inode, err := parseInode(buf, v.ioh.blockSize, v.inodeFeatures())
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

	v.cacheInode(inodeNumber, inode)

	return shallowCopyInode(inode), nil
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

		// An XFS extent never spans an allocation group, so advancing the
		// group-relative part of the block number stays inside the group.
		physicalBlock := extent.PhysicalBlockNumber + extentBlockIndex
		blockOffset, err := v.fileSystemBlockOffset(physicalBlock)
		if err != nil {
			return written, err
		}
		diskOff := blockOffset + int64(inBlockOffset)
		if err := v.readAt(p[written:written+int(toRead)], diskOff); err != nil {
			return written, wrapIOError("read", diskOff, int(toRead), err)
		}
		written += int(toRead)
	}

	if written < len(p) {
		return written, io.EOF
	}
	return written, nil
}

// fileSystemBlockOffset converts an XFS filesystem block number to a byte
// offset within the volume.
//
// An fsblock is not a linear block index. It packs the allocation group number
// into the high bits and the group-relative block number into the low
// sb_agblklog bits. The stride between groups on disk is sb_agblocks, which
// equals 1<<sb_agblklog only when the group size happens to be a power of two,
// so reading an fsblock as though it were linear addresses the wrong data for
// every group above the first, and runs off the end of the volume near the
// last one. Group zero is the only one where the two happen to coincide, which
// is why this is invisible on small single-group images.
func (v *Volume) fileSystemBlockOffset(fileSystemBlock uint64) (int64, error) {
	if v.ioh.blockSize == 0 {
		return 0, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}
	bits := v.ioh.relativeBlockNumberBits
	if v.ioh.allocationGroupSize == 0 || bits == 0 || bits >= 64 {
		return 0, wrapParseError(int64(bits), "allocation_group_geometry", ErrInvalidSuperblock)
	}

	allocationGroupIndex := fileSystemBlock >> bits
	relativeBlockNumber := fileSystemBlock & ((uint64(1) << bits) - 1)

	linearBlock := allocationGroupIndex*uint64(v.ioh.allocationGroupSize) + relativeBlockNumber
	if linearBlock > uint64(math.MaxInt64)/uint64(v.ioh.blockSize) {
		return 0, wrapParseError(int64(fileSystemBlock), "file_system_block", ErrInvalidInode)
	}

	// The resolved block is bounded against the volume rather than against
	// sb_agblocks. Both sb_agblocks and sb_agblklog are attacker controlled on
	// a crafted image, so treating their disagreement as fatal would reject
	// recoverable data without preventing any access the volume bound does not
	// already prevent.
	if v.sb != nil && v.sb.NumberOfBlocks != 0 && linearBlock >= v.sb.NumberOfBlocks {
		return 0, wrapParseError(int64(fileSystemBlock), "file_system_block", ErrInvalidInode)
	}
	return int64(linearBlock * uint64(v.ioh.blockSize)), nil
}

// findExtentForBlock locates the extent covering a logical block, and when
// there is none, the start of the next mapped extent so the caller can size
// the hole.
//
// extents must be sorted by logical block, which is how they are stored on
// disk and how normalizeExtents leaves them. The search is binary
// because it runs once per read chunk: scanning linearly made reading a file
// with many extents quadratic in the extent count.
func findExtentForBlock(extents []Extent, logicalBlock uint64) (*Extent, uint64) {
	// First extent starting strictly after logicalBlock.
	next := sort.Search(len(extents), func(i int) bool {
		return extents[i].LogicalBlockNumber > logicalBlock
	})

	// Only the extent immediately before it can contain the block.
	if next > 0 {
		candidate := &extents[next-1]
		end := candidate.LogicalBlockNumber + uint64(candidate.NumberOfBlocks)
		if logicalBlock >= candidate.LogicalBlockNumber && logicalBlock < end {
			return candidate, math.MaxUint64
		}
	}
	if next < len(extents) {
		return nil, extents[next].LogicalBlockNumber
	}
	return nil, math.MaxUint64
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
		if inode.AttributeExtentCount == 0 {
			return nil
		}
		extents, err := parseExtentList(inode.AttributeExtentCount, data)
		if err != nil {
			return err
		}
		extents = normalizeExtents(extents)
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

	extents, err := v.getExtentsFromExtentBtreeBranchNode(numberOfRecords, data[4:], int(level), 0)
	if err != nil {
		return nil, err
	}

	// The whole fork is in hand only here, which is the only place holes can
	// be located correctly.
	extents = normalizeExtents(extents)
	if addSparseExtents {
		extents = fillSparseExtents(extents, numberOfBlocks)
	}
	return extents, nil
}

func (v *Volume) getExtentsFromExtentBtreeBranchNode(numberOfRecords uint16, recordsData []byte, maximumDepth int, recursionDepth int) ([]Extent, error) {
	if recursionDepth < 0 || recursionDepth > maxBtreeRecursionDepth {
		return nil, wrapParseError(0, "extent_btree_recursion_depth", ErrInvalidInode)
	}

	numberOfKeyValuePairs := len(recordsData) / extentBtreeKeyPointerPairSize
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

		extents, err := v.getExtentsFromExtentBtreeNode(subBlock, maximumDepth, recursionDepth+1)
		if err != nil {
			return nil, err
		}
		result = append(result, extents...)
	}

	return result, nil
}

func (v *Volume) getExtentsFromExtentBtreeNode(blockNumber uint64, maximumDepth int, recursionDepth int) ([]Extent, error) {
	if v.ioh.allocationGroupSize == 0 {
		return nil, wrapParseError(0, "allocation_group_size", ErrInvalidSuperblock)
	}
	if v.ioh.blockSize == 0 {
		return nil, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}
	if recursionDepth < 0 || recursionDepth > maxBtreeRecursionDepth {
		return nil, wrapParseError(0, "extent_btree_recursion_depth", ErrInvalidInode)
	}

	offset, err := v.fileSystemBlockOffset(blockNumber)
	if err != nil {
		return nil, err
	}

	blockData := make([]byte, v.ioh.blockSize)
	if err := v.readAt(blockData, offset); err != nil {
		return nil, wrapIOError("read", offset, len(blockData), err)
	}

	header, headerSize, err := parseBtreeBlockHeader(blockData, v.ioh.formatVersion, 8)
	if err != nil {
		return nil, err
	}
	recordsData := blockData[headerSize:]

	if v.ioh.formatVersion == sbFormatVersion5 {
		if header.Signature != extentBtreeMagicV5 {
			return nil, wrapParseError(offset, "extent_btree_signature", ErrInvalidInode)
		}
	} else {
		if header.Signature != extentBtreeMagicV4 {
			return nil, wrapParseError(offset, "extent_btree_signature", ErrInvalidInode)
		}
	}

	if int(header.Level) > maximumDepth {
		return nil, wrapParseError(offset, "extent_btree_level", ErrInvalidInode)
	}

	if header.Level == 0 {
		return parseExtentList(uint32(header.NumberOfRecords), recordsData)
	}

	return v.getExtentsFromExtentBtreeBranchNode(header.NumberOfRecords, recordsData, maximumDepth, recursionDepth)
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
	if err := v.readAt(blockData, offset); err != nil {
		return false, wrapIOError("read", offset, len(blockData), err)
	}

	header, headerSize, err := parseBtreeBlockHeader(blockData, v.ioh.formatVersion, 4)
	if err != nil {
		return false, err
	}
	recordsData := blockData[headerSize:]

	if v.ioh.formatVersion == sbFormatVersion5 {
		if header.Signature != inodeBtreeMagicV5 {
			return false, wrapParseError(offset, "btree_signature", ErrInvalidInode)
		}
	} else {
		if header.Signature != inodeBtreeMagicV4 {
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
	if blockNumberDataSize != btreePointerSizeShort && blockNumberDataSize != btreePointerSizeLong {
		return nil, 0, wrapParseError(0, "block_number_data_size", ErrInvalidInode)
	}

	longPointers := blockNumberDataSize == btreePointerSizeLong
	headerSize := btreeHeaderSizeShortV4
	switch {
	case formatVersion == sbFormatVersion5 && longPointers:
		headerSize = btreeHeaderSizeLongV5
	case formatVersion == sbFormatVersion5:
		headerSize = btreeHeaderSizeShortV5
	case longPointers:
		headerSize = btreeHeaderSizeLongV4
	}
	if len(data) < headerSize {
		return nil, 0, wrapParseError(0, "btree_header", ErrInvalidInode)
	}

	level, ok := readUint16BE(data, btreeOffsetLevel)
	if !ok {
		return nil, 0, wrapParseError(btreeOffsetLevel, "btree_level", ErrInvalidInode)
	}
	numberOfRecords, ok := readUint16BE(data, btreeOffsetRecordCount)
	if !ok {
		return nil, 0, wrapParseError(btreeOffsetRecordCount, "btree_number_of_records", ErrInvalidInode)
	}

	return &btreeBlockHeader{
		Signature:       string(data[btreeOffsetMagic : btreeOffsetMagic+btreeMagicLength]),
		Level:           level,
		NumberOfRecords: numberOfRecords,
	}, headerSize, nil
}

func inodeFoundInLeafRecords(recordsData []byte, numberOfRecords uint16, relativeInodeNumber uint64, offset int64) (bool, error) {
	if int(numberOfRecords) > len(recordsData)/inobtRecordSize {
		return false, wrapParseError(offset, "leaf_number_of_records", ErrInvalidInode)
	}
	for i := 0; i < int(numberOfRecords); i++ {
		recordOffset := i * inobtRecordSize
		inodeNumber, ok := readUint32BE(recordsData, recordOffset+inobtRecordOffsetStartInode)
		if !ok {
			return false, wrapParseError(offset+int64(recordOffset), "leaf_inode_number", ErrInvalidInode)
		}
		numberOfUnusedInodes, ok := readUint32BE(recordsData, recordOffset+inobtRecordOffsetFreeCount)
		if !ok {
			return false, wrapParseError(offset+int64(recordOffset+4), "leaf_unused_inode_count", ErrInvalidInode)
		}
		chunkAllocationBitmap, ok := readUint64BE(recordsData, recordOffset+inobtRecordOffsetFreeMask)
		if !ok {
			return false, wrapParseError(offset+int64(recordOffset+8), "leaf_chunk_bitmap", ErrInvalidInode)
		}
		if relativeInodeNumber >= uint64(inodeNumber) &&
			relativeInodeNumber < uint64(inodeNumber)+inobtInodesPerChunk {
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
	if relativeInodeNumber < chunkBaseInode ||
		relativeInodeNumber >= chunkBaseInode+inobtInodesPerChunk {
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
	if err := v.readAt(sbData, 0); err != nil {
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
		bigTime:                 sb.HasBigTimestamps(),
		largeExtentCounts:       sb.HasLargeExtentCounts(),
	}

	v.agInode = make([]InodeInformation, 0, sb.NumberOfAllocationGroups)
	for ag := uint32(0); ag < sb.NumberOfAllocationGroups; ag++ {
		superblockOffset := int64(ag) * int64(sb.AllocationGroupSize) * int64(sb.BlockSize)
		if err := v.readAt(sbData, superblockOffset); err != nil {
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
		if err := v.readAt(agiData, inodeInfoOffset); err != nil {
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

// isValidSectorSize reports whether a sector size is one XFS can use: a power
// of two between the minimum and maximum the format allows.
func isValidSectorSize(sectorSize uint16) bool {
	if sectorSize < minSectorSize || sectorSize > maxSectorSize {
		return false
	}
	return sectorSize&(sectorSize-1) == 0
}

// directoryBlockSizeFromLog derives the directory block size from
// sb_dirblklog, which counts filesystem blocks per directory block as a power
// of two.
func directoryBlockSizeFromLog(blockSize uint32, directoryBlockLog uint8) (uint32, error) {
	if directoryBlockLog == 0 {
		return blockSize, nil
	}
	if uint32(directoryBlockLog) >= maxDirectoryBlockLog {
		return 0, wrapParseError(sbOffsetDirectoryBlockLog, "directory_block_size_log2", ErrInvalidSuperblock)
	}

	multiplier := uint32(1) << directoryBlockLog
	if multiplier > math.MaxUint32/blockSize {
		return 0, wrapParseError(sbOffsetDirectoryBlockLog, "directory_block_size_log2", ErrInvalidSuperblock)
	}
	return multiplier * blockSize, nil
}

func parseSuperblock(data []byte) (*Superblock, error) {
	if len(data) < superblockSize {
		return nil, wrapParseError(0, "superblock", ErrInvalidSuperblock)
	}
	if !bytes.Equal(data[sbOffsetMagic:sbOffsetMagic+len(xfsSuperblockMagic)], []byte(xfsSuperblockMagic)) {
		return nil, wrapParseError(sbOffsetMagic, "signature", ErrInvalidSuperblock)
	}

	blockSize, _ := readUint32BE(data, sbOffsetBlockSize)
	numberOfBlocks, _ := readUint64BE(data, sbOffsetDataBlocks)
	journalBlockNumber, _ := readUint64BE(data, sbOffsetLogStart)
	rootInode, _ := readUint64BE(data, sbOffsetRootInode)
	allocationGroupSize, _ := readUint32BE(data, sbOffsetAGBlocks)
	numberOfAllocationGroups, _ := readUint32BE(data, sbOffsetAGCount)
	versionAndFlags, _ := readUint16BE(data, sbOffsetVersionNumber)
	sectorSize, _ := readUint16BE(data, sbOffsetSectorSize)
	inodeSize, _ := readUint16BE(data, sbOffsetInodeSize)
	numberOfInodesPerBlock, _ := readUint16BE(data, sbOffsetInodesPerBlock)
	secondaryFeatureFlags, _ := readUint32BE(data, sbOffsetSecondaryFeatureFlags)

	var volumeLabel [sbFilesystemNameLength]byte
	copy(volumeLabel[:], data[sbOffsetFilesystemName:sbOffsetFilesystemName+sbFilesystemNameLength])

	formatVersion := uint8(versionAndFlags & sbVersionMask)
	featureFlags := versionAndFlags & sbFeatureFlagsMask

	if formatVersion != sbFormatVersion4 && formatVersion != sbFormatVersion5 {
		return nil, wrapParseError(sbOffsetVersionNumber, "format_version", ErrInvalidSuperblock)
	}

	if featureFlags&^sbSupportedFeatureFlags != 0 {
		return nil, wrapParseError(sbOffsetVersionNumber, "feature_flags", ErrUnsupportedFeatureFlag)
	}

	if blockSize < minBlockSize || blockSize > maxBlockSize {
		return nil, wrapParseError(sbOffsetBlockSize, "block_size", ErrInvalidSuperblock)
	}
	if !isValidSectorSize(sectorSize) {
		return nil, wrapParseError(sbOffsetSectorSize, "sector_size", ErrInvalidSuperblock)
	}
	if inodeSize < minInodeSize || inodeSize > maxInodeSize {
		return nil, wrapParseError(sbOffsetInodeSize, "inode_size", ErrInvalidSuperblock)
	}

	directoryBlockSize, err := directoryBlockSizeFromLog(blockSize, data[sbOffsetDirectoryBlockLog])
	if err != nil {
		return nil, err
	}

	if allocationGroupSize < minAllocationGroupBlocks || allocationGroupSize > math.MaxInt32 {
		return nil, wrapParseError(sbOffsetAGBlocks, "allocation_group_size", ErrInvalidSuperblock)
	}
	allocationGroupSizeLog2 := data[sbOffsetAGBlocksLog]
	if allocationGroupSizeLog2 == 0 || allocationGroupSizeLog2 > maxAllocationGroupLog {
		return nil, wrapParseError(sbOffsetAGBlocksLog, "allocation_group_size_log2", ErrInvalidSuperblock)
	}

	numberOfInodesPerBlockLog2 := data[sbOffsetInodesPerBlockLog]
	relativeBlockBits := allocationGroupSizeLog2
	if numberOfInodesPerBlockLog2 == 0 ||
		int(numberOfInodesPerBlockLog2) > (inodeAddressBits-int(relativeBlockBits)) {
		return nil, wrapParseError(sbOffsetInodesPerBlockLog, "number_of_inodes_per_block_log2", ErrInvalidSuperblock)
	}
	relativeInodeBits := relativeBlockBits + numberOfInodesPerBlockLog2
	if relativeInodeBits == 0 || relativeInodeBits >= maxRelativeInodeBits {
		return nil, wrapParseError(sbOffsetInodesPerBlockLog, "relative_inode_number_bits", ErrInvalidSuperblock)
	}
	if uint16(1)<<numberOfInodesPerBlockLog2 != numberOfInodesPerBlock {
		return nil, wrapParseError(sbOffsetInodesPerBlock, "number_of_inodes_per_block", ErrInvalidSuperblock)
	}

	// The feature words only exist on v5 superblocks; on v4 the space holds
	// unrelated data and must not be interpreted.
	var featuresCompat, featuresReadOnly, featuresIncompat, featuresLogIncompat uint32
	if formatVersion == sbFormatVersion5 {
		featuresCompat, _ = readUint32BE(data, superblockFeaturesCompatOffset)
		featuresReadOnly, _ = readUint32BE(data, superblockFeaturesReadOnlyOffset)
		featuresIncompat, _ = readUint32BE(data, superblockFeaturesIncompatOffset)
		featuresLogIncompat, _ = readUint32BE(data, superblockFeaturesLogIncompatOffset)

		// An unrecognised incompatible feature changes the on-disk layout in
		// a way this parser cannot account for. Reading on would silently
		// produce wrong metadata, which is worse than refusing.
		if unknown := featuresIncompat &^ knownFeaturesIncompat; unknown != 0 {
			return nil, fmt.Errorf("%w: unknown incompatible feature bits 0x%08x",
				ErrUnsupportedFeatureFlag, unknown)
		}
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
		FeaturesCompat:           featuresCompat,
		FeaturesReadOnlyCompat:   featuresReadOnly,
		FeaturesIncompat:         featuresIncompat,
		FeaturesLogIncompat:      featuresLogIncompat,
	}, nil
}

func parseInodeInformation(data []byte, formatVersion uint8) (*InodeInformation, error) {
	required := agiSizeV4
	if formatVersion >= sbFormatVersion5 {
		required = agiSizeV5
	}
	if len(data) < required {
		return nil, wrapParseError(0, "inode_information", ErrInvalidInodeInfo)
	}
	if !bytes.Equal(data[agiOffsetMagic:agiOffsetMagic+len(xfsAGIMagic)], []byte(xfsAGIMagic)) {
		return nil, wrapParseError(agiOffsetMagic, "signature", ErrInvalidInodeInfo)
	}

	fv, _ := readUint32BE(data, agiOffsetVersion)
	root, _ := readUint32BE(data, agiOffsetInodeBtreeRoot)
	depth, _ := readUint32BE(data, agiOffsetInodeBtreeDepth)
	lastChunk, _ := readUint32BE(data, agiOffsetLastAllocatedChunk)

	return &InodeInformation{
		FormatVersion:       fv,
		InodeBtreeRootBlock: root,
		InodeBtreeDepth:     depth,
		LastAllocatedChunk:  lastChunk,
	}, nil
}

// inodeFeatures carries the superblock feature bits that change how an inode
// is laid out on disk.
type inodeFeatures struct {
	bigTime           bool
	largeExtentCounts bool
}

func parseInode(data []byte, blockSize uint32, features inodeFeatures) (*Inode, error) {
	if len(data) < minInodeSize {
		return nil, wrapParseError(0, "inode", ErrInvalidInode)
	}
	if !bytes.Equal(data[inodeOffsetMagic:inodeOffsetMagic+len(xfsInodeMagic)], []byte(xfsInodeMagic)) {
		return nil, wrapParseError(inodeOffsetMagic, "signature", ErrInvalidInode)
	}

	formatVersion := data[inodeOffsetVersion]
	inodeHeaderSize := inodeHeaderSizeV2
	if formatVersion == inodeVersion3 {
		inodeHeaderSize = inodeHeaderSizeV3
	}
	if len(data) < inodeHeaderSize {
		return nil, wrapParseError(0, "inode_header", ErrInvalidInode)
	}
	if formatVersion != inodeVersion1 && formatVersion != inodeVersion2 && formatVersion != inodeVersion3 {
		return nil, wrapParseError(inodeOffsetVersion, "format_version", ErrInvalidInode)
	}

	fileMode, _ := readUint16BE(data, inodeOffsetMode)
	forkType := data[inodeOffsetFormat]
	ownerID, _ := readUint32BE(data, inodeOffsetUserID)
	groupID, _ := readUint32BE(data, inodeOffsetGroupID)

	var numberOfLinks uint32
	if formatVersion == inodeVersion1 {
		v, _ := readUint16BE(data, inodeOffsetLinkCountV1)
		numberOfLinks = uint32(v)
	} else {
		numberOfLinks, _ = readUint32BE(data, inodeOffsetLinkCount)
	}

	// Bigtime only applies to v3 inodes; v1/v2 inodes always use the legacy
	// encoding regardless of what the superblock advertises.
	bigTime := features.bigTime && formatVersion == inodeVersion3

	size, _ := readUint64BE(data, inodeOffsetSize)
	attrForkOffset := uint16(data[inodeOffsetAttributeForkShift]) * inodeAttributeForkShiftUnit
	attrForkType := data[inodeOffsetAttributeFormat]

	// With the nrext64 feature the extent counters move and widen: the data
	// fork count becomes a 64-bit value in the padding at offset 24, and the
	// attribute fork count becomes a 32-bit value at offset 76.
	var dataExtentCount uint64
	var attrExtentCount uint32
	if features.largeExtentCounts {
		dataExtentCount, _ = readUint64BE(data, inodeOffsetBigExtentCount)
		attrExtentCount, _ = readUint32BE(data, inodeOffsetExtentCount)
	} else {
		narrowData, _ := readUint32BE(data, inodeOffsetExtentCount)
		narrowAttr, _ := readUint16BE(data, inodeOffsetAttributeExtents)
		dataExtentCount = uint64(narrowData)
		attrExtentCount = uint32(narrowAttr)
	}

	inode := &Inode{
		FormatVersion:            formatVersion,
		FileMode:                 fileMode,
		ForkType:                 forkType,
		OwnerID:                  ownerID,
		GroupID:                  groupID,
		NumberOfLinks:            numberOfLinks,
		AccessTimeNS:             readInodeTimestamp(data, inodeOffsetAccessTime, bigTime),
		ModificationTimeNS:       readInodeTimestamp(data, inodeOffsetModificationTime, bigTime),
		InodeChangeTimeNS:        readInodeTimestamp(data, inodeOffsetChangeTime, bigTime),
		Size:                     size,
		NumberOfDataExtents:      saturateUint32(dataExtentCount),
		NumberOfAttributesExtent: saturateUint16(attrExtentCount),
		DataExtentCount:          dataExtentCount,
		AttributeExtentCount:     attrExtentCount,
		HasBigTimestamps:         bigTime,
		AttributesForkType:       attrForkType,
		Raw:                      append([]byte(nil), data...),
	}

	if formatVersion == inodeVersion3 {
		inode.CreationTimeNS = readInodeTimestamp(data, inodeOffsetCreationTime, bigTime)
	}

	dataForkSize := len(data) - inodeHeaderSize
	if attrForkOffset > 0 {
		if int(attrForkOffset) >= dataForkSize {
			return nil, wrapParseError(inodeOffsetAttributeForkShift, "attributes_fork_offset", ErrInvalidInode)
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
		if int(inode.DataForkOffset)+deviceIdentifierSize > len(data) {
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
		numberOfExtents := saturateUint32(inode.DataExtentCount)
		if numberOfExtents == 0 {
			numberOfExtents = inferExtentCount(data[start:end])
		}
		if numberOfExtents == 0 {
			return inode, nil
		}

		extents, err := parseExtentList(numberOfExtents, data[start:end])
		if err != nil {
			return nil, err
		}
		extents = normalizeExtents(extents)
		if addSparseExtents {
			extents = fillSparseExtents(extents, numberOfBlocks)
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
