package libxfs

import (
	"encoding/binary"
	"testing"
)

// Synthetic XFS image builder for directory tests.
//
// Existing tests poke absolute byte offsets by hand, which is why no v5
// directory fixture existed. This builder composes a superblock, AGI, inode
// b-tree and inodes so that a test can describe a directory rather than a
// byte layout.

const (
	fixtureBlockSize         = 4096
	fixtureInodeSize         = 256
	fixtureInodesPerBlock    = 16
	fixtureInodesPerBlockLog = 4
	fixtureAGBlocksLog       = 8
	fixtureAGBlocks          = 1 << fixtureAGBlocksLog

	fixtureRootInode  = 32
	fixtureFirstInode = 33
)

// Header sizes derived independently from the on-disk struct definitions in
// xfs_da_format.h.
//
// Fixtures must NOT reuse the parser's own constants. If both sides shared a
// value, a regression in the parser would be mirrored by the fixture and the
// test would keep passing against a broken parser.
const (
	// xfs_dir3_blk_hdr: magic(4) + crc(4) + blkno(8) + lsn(8) + uuid(16) + owner(8)
	fixtureDir3BlockHeaderSize = 4 + 4 + 8 + 8 + 16 + 8
	// xfs_dir3_data_hdr: xfs_dir3_blk_hdr + best_free[3] + pad(4)
	fixtureDir3DataHeaderSize = fixtureDir3BlockHeaderSize + 3*4 + 4
	// xfs_dir2_data_hdr: magic(4) + bestfree[3]
	fixtureDir2DataHeaderSize = 4 + 3*4
)

// fixtureEntryLength computes an entry's on-disk size independently of the
// parser: inumber(8) + namelen(1) + name + optional ftype(1) + tag(2), rounded
// up to an 8 byte boundary.
func fixtureEntryLength(nameLength int, hasFileType bool) int {
	length := 8 + 1 + nameLength + 2
	if hasFileType {
		length++
	}
	if remainder := length % 8; remainder != 0 {
		length += 8 - remainder
	}
	return length
}

type fixtureImage struct {
	data          []byte
	formatVersion uint8
	inodeVersion  uint8
	blockSize     uint32
	dirBlockSize  uint32
	// nextBlock is the next free filesystem block for payload allocation.
	// Blocks 0..7 are reserved for the superblock, AGI and inode area.
	nextBlock        uint64
	featuresIncompat uint32
}

// newFixtureImage builds an empty image. dirBlockLog is log2 of the number of
// filesystem blocks per directory block (0 => directory block == fs block).
func newFixtureImage(formatVersion uint8, dirBlockLog uint8) *fixtureImage {
	image := &fixtureImage{
		data:          make([]byte, fixtureAGBlocks*fixtureBlockSize),
		formatVersion: formatVersion,
		blockSize:     fixtureBlockSize,
		dirBlockSize:  fixtureBlockSize << dirBlockLog,
		nextBlock:     8,
	}
	image.inodeVersion = 3
	if formatVersion == 4 {
		image.inodeVersion = 2
	}

	sb := image.data
	copy(sb[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(sb[4:8], fixtureBlockSize)
	binary.BigEndian.PutUint64(sb[8:16], fixtureAGBlocks)
	binary.BigEndian.PutUint64(sb[48:56], 32)
	binary.BigEndian.PutUint64(sb[56:64], fixtureRootInode)
	binary.BigEndian.PutUint32(sb[84:88], fixtureAGBlocks)
	binary.BigEndian.PutUint32(sb[88:92], 1)
	binary.BigEndian.PutUint16(sb[100:102], 0x0010|uint16(formatVersion))
	binary.BigEndian.PutUint16(sb[102:104], 512)
	binary.BigEndian.PutUint16(sb[104:106], fixtureInodeSize)
	binary.BigEndian.PutUint16(sb[106:108], fixtureInodesPerBlock)
	copy(sb[108:120], []byte("FIXTURE"))
	sb[123] = fixtureInodesPerBlockLog
	sb[124] = fixtureAGBlocksLog
	sb[192] = dirBlockLog

	// AGI at sector 2, with the inode b-tree root at block 5.
	agi := 2 * 512
	copy(sb[agi:agi+4], []byte("XAGI"))
	binary.BigEndian.PutUint32(sb[agi+4:agi+8], 1)
	binary.BigEndian.PutUint32(sb[agi+20:agi+24], 5)
	binary.BigEndian.PutUint32(sb[agi+24:agi+28], 1)
	binary.BigEndian.PutUint32(sb[agi+32:agi+36], 0)

	// Inode b-tree root: one leaf record marking inodes [0, 64) allocated.
	btree := 5 * fixtureBlockSize
	recordOffset := btree + 56
	copy(sb[btree:btree+4], []byte("IAB3"))
	if formatVersion == 4 {
		copy(sb[btree:btree+4], []byte("IABT"))
		recordOffset = btree + 16
	}
	binary.BigEndian.PutUint16(sb[btree+4:btree+6], 0)
	binary.BigEndian.PutUint16(sb[btree+6:btree+8], 1)
	binary.BigEndian.PutUint32(sb[recordOffset:recordOffset+4], 0)
	binary.BigEndian.PutUint32(sb[recordOffset+4:recordOffset+8], 0)
	binary.BigEndian.PutUint64(sb[recordOffset+8:recordOffset+16], 0)

	return image
}

// setFeaturesIncompat sets the v5 sb_features_incompat word.
func (f *fixtureImage) setFeaturesIncompat(features uint32) {
	f.featuresIncompat = features
	binary.BigEndian.PutUint32(f.data[216:220], features)
}

func (f *fixtureImage) hasBigTime() bool {
	return f.featuresIncompat&FeatureIncompatBigTime != 0
}

func (f *fixtureImage) hasLargeExtentCounts() bool {
	return f.featuresIncompat&FeatureIncompatLargeExtentCounts != 0
}

// writeInodeTimestamps stamps all four inode timestamps, encoding them the way
// the image's feature flags say they should be stored.
func (f *fixtureImage) writeInodeTimestamps(inodeNumber uint64, unixNanos int64) {
	base := inodeNumber * fixtureInodeSize
	for _, offset := range []uint64{32, 40, 48, 144} {
		at := base + offset
		if f.hasBigTime() {
			// bigtime counts nanoseconds from 1901-12-13 20:45:52 UTC.
			raw := uint64(unixNanos/1_000_000_000+fixtureBigTimeEpochOffset)*1_000_000_000 +
				uint64(unixNanos%1_000_000_000)
			binary.BigEndian.PutUint64(f.data[at:at+8], raw)
			continue
		}
		binary.BigEndian.PutUint32(f.data[at:at+4], uint32(unixNanos/1_000_000_000))
		binary.BigEndian.PutUint32(f.data[at+4:at+8], uint32(unixNanos%1_000_000_000))
	}
}

// fixtureBigTimeEpochOffset is derived from the format definition rather than
// reused from the parser, so a regression in the parser cannot be mirrored by
// the fixture. It is -(int64)S32_MIN.
const fixtureBigTimeEpochOffset = int64(1) << 31

// hasFileType reports whether entries in this image carry an ftype byte.
func (f *fixtureImage) hasFileType() bool {
	return f.formatVersion == 5
}

func (f *fixtureImage) inodeHeaderSize() int {
	if f.inodeVersion == 3 {
		return 176
	}
	return 100
}

// allocateBlocks reserves count contiguous filesystem blocks.
func (f *fixtureImage) allocateBlocks(count uint64) uint64 {
	start := f.nextBlock
	f.nextBlock += count
	if f.nextBlock*uint64(f.blockSize) > uint64(len(f.data)) {
		panic("fixture image out of space")
	}
	return start
}

func (f *fixtureImage) writeAt(offset uint64, data []byte) {
	copy(f.data[offset:offset+uint64(len(data))], data)
}

// writeInode writes a minimal inode header.
func (f *fixtureImage) writeInode(inodeNumber uint64, mode uint16, forkType uint8, size uint64, extentCount uint32) uint64 {
	offset := inodeNumber * fixtureInodeSize
	buf := f.data

	copy(buf[offset:offset+2], []byte("IN"))
	binary.BigEndian.PutUint16(buf[offset+2:offset+4], mode)
	buf[offset+4] = f.inodeVersion
	buf[offset+5] = forkType
	binary.BigEndian.PutUint32(buf[offset+8:offset+12], 1000)
	binary.BigEndian.PutUint32(buf[offset+12:offset+16], 1000)
	binary.BigEndian.PutUint32(buf[offset+16:offset+20], 2)
	binary.BigEndian.PutUint64(buf[offset+56:offset+64], size)
	if f.hasLargeExtentCounts() {
		// nrext64 moves the data fork count to a 64-bit field at offset 24 and
		// widens the attribute fork count to 32 bits at offset 76.
		binary.BigEndian.PutUint64(buf[offset+24:offset+32], uint64(extentCount))
		binary.BigEndian.PutUint32(buf[offset+76:offset+80], 0)
	} else {
		binary.BigEndian.PutUint32(buf[offset+76:offset+80], extentCount)
		binary.BigEndian.PutUint16(buf[offset+80:offset+82], 0)
	}
	buf[offset+82] = 0
	buf[offset+83] = 0

	return offset + uint64(f.inodeHeaderSize())
}

// addShortFormDirectory writes an inline short-form directory inode.
func (f *fixtureImage) addShortFormDirectory(inodeNumber uint64, parentInode uint64, entries []fixtureEntry) {
	payload := []byte{byte(len(entries)), 0}
	parent := make([]byte, 4)
	binary.BigEndian.PutUint32(parent, uint32(parentInode))
	payload = append(payload, parent...)

	for _, entry := range entries {
		payload = append(payload, byte(len(entry.name)), 0, 0)
		payload = append(payload, entry.name...)
		if f.hasFileType() {
			payload = append(payload, entry.fileType)
		}
		child := make([]byte, 4)
		binary.BigEndian.PutUint32(child, uint32(entry.inodeNumber))
		payload = append(payload, child...)
	}

	forkOffset := f.writeInode(inodeNumber, FileTypeDirectory|0755, ForkTypeInlineData, uint64(len(payload)), 0)
	f.writeAt(forkOffset, payload)
}

// addBlockDirectory writes a directory inode backed by the supplied directory
// blocks, laid out contiguously starting at a freshly allocated block.
func (f *fixtureImage) addBlockDirectory(inodeNumber uint64, blocks [][]byte) {
	blocksPerDirBlock := uint64(f.dirBlockSize) / uint64(f.blockSize)
	totalBlocks := uint64(len(blocks)) * blocksPerDirBlock
	startBlock := f.allocateBlocks(totalBlocks)

	for i, block := range blocks {
		offset := (startBlock + uint64(i)*blocksPerDirBlock) * uint64(f.blockSize)
		f.writeAt(offset, block)
	}

	size := uint64(len(blocks)) * uint64(f.dirBlockSize)
	forkOffset := f.writeInode(inodeNumber, FileTypeDirectory|0755, ForkTypeExtents, size, 1)
	extent := encodeExtent(0, startBlock, uint32(totalBlocks), false)
	f.writeAt(forkOffset, extent[:])
}

// addBtreeDirectory writes a directory whose data fork is an extent b-tree
// rather than an inline extent list.
//
// The inode holds a bmbt root pointing at one BMA3/BMAP node block, which in
// turn holds the extent mapping the directory's data blocks. This is the
// layout XFS uses once a directory outgrows the inode's extent list.
func (f *fixtureImage) addBtreeDirectory(inodeNumber uint64, blocks [][]byte) {
	blocksPerDirBlock := uint64(f.dirBlockSize) / uint64(f.blockSize)
	totalBlocks := uint64(len(blocks)) * blocksPerDirBlock

	dataStart := f.allocateBlocks(totalBlocks)
	for i, block := range blocks {
		offset := (dataStart + uint64(i)*blocksPerDirBlock) * uint64(f.blockSize)
		f.writeAt(offset, block)
	}

	nodeBlock := f.allocateBlocks(1)
	nodeOffset := nodeBlock * uint64(f.blockSize)

	signature := "BMA3"
	nodeHeaderSize := uint64(72)
	if f.formatVersion == 4 {
		signature = "BMAP"
		nodeHeaderSize = 24
	}
	f.writeAt(nodeOffset, []byte(signature))
	binary.BigEndian.PutUint16(f.data[nodeOffset+4:nodeOffset+6], 0) // leaf level
	binary.BigEndian.PutUint16(f.data[nodeOffset+6:nodeOffset+8], 1) // one record

	extent := encodeExtent(0, dataStart, uint32(totalBlocks), false)
	f.writeAt(nodeOffset+nodeHeaderSize, extent[:])

	size := uint64(len(blocks)) * uint64(f.dirBlockSize)
	forkOffset := f.writeInode(inodeNumber, FileTypeDirectory|0755, ForkTypeBtree, size, 1)

	// bmbt root: level 1, one key/pointer pair. The pointer array follows the
	// key array, which is sized from the whole remaining fork space.
	forkSize := uint64(fixtureInodeSize) - uint64(f.inodeHeaderSize())
	binary.BigEndian.PutUint16(f.data[forkOffset:forkOffset+2], 1)
	binary.BigEndian.PutUint16(f.data[forkOffset+2:forkOffset+4], 1)
	pointerOffset := forkOffset + 4 + (forkSize-4)/16*8
	binary.BigEndian.PutUint64(f.data[pointerOffset:pointerOffset+8], nodeBlock)
}

// setAttributeExtentCount writes the attribute fork extent count at whichever
// offset the image's feature flags dictate.
func (f *fixtureImage) setAttributeExtentCount(inodeNumber uint64, count uint32) {
	offset := inodeNumber * fixtureInodeSize
	if f.hasLargeExtentCounts() {
		binary.BigEndian.PutUint32(f.data[offset+76:offset+80], count)
		return
	}
	binary.BigEndian.PutUint16(f.data[offset+80:offset+82], uint16(count))
}

// setInodeSize overrides a directory inode's recorded size, for testing how
// the walker reacts to an implausible or unaligned di_size.
func (f *fixtureImage) setInodeSize(inodeNumber uint64, size uint64) {
	offset := inodeNumber * fixtureInodeSize
	binary.BigEndian.PutUint64(f.data[offset+56:offset+64], size)
}

func (f *fixtureImage) open(t *testing.T) *Volume {
	t.Helper()
	volume, err := Open(&mockReaderAt{data: f.data})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	return volume
}

type fixtureEntry struct {
	name        string
	inodeNumber uint64
	fileType    uint8
}

// directoryDataMagic returns the data-block magic for this image.
func (f *fixtureImage) directoryDataMagic() string {
	if f.formatVersion == 5 {
		return dirDataMagicV5
	}
	return dirDataMagicV4
}

// directoryBlockMagic returns the block-format magic for this image.
func (f *fixtureImage) directoryBlockMagic() string {
	if f.formatVersion == 5 {
		return dirBlockMagicV5
	}
	return dirBlockMagicV4
}

// buildDirectoryBlock lays out one directory block containing entries, with
// all remaining space marked as a single free run.
func (f *fixtureImage) buildDirectoryBlock(magic string, entries []fixtureEntry) []byte {
	block := make([]byte, f.dirBlockSize)
	copy(block[0:4], magic)

	headerSize := fixtureDir2DataHeaderSize
	if magic == dirDataMagicV5 || magic == dirBlockMagicV5 {
		headerSize = fixtureDir3DataHeaderSize
	}

	entriesEnd := len(block)
	if magic == dirBlockMagicV4 || magic == dirBlockMagicV5 {
		// Block format keeps a leaf array and an 8 byte tail at the end.
		entriesEnd = len(block) - 8 - len(entries)*8
		binary.BigEndian.PutUint32(block[len(block)-8:len(block)-4], uint32(len(entries)))
		binary.BigEndian.PutUint32(block[len(block)-4:], 0)
	}

	offset := headerSize
	for _, entry := range entries {
		recordLength := fixtureEntryLength(len(entry.name), f.hasFileType())
		if offset+recordLength > entriesEnd {
			panic("fixture directory block overflow")
		}
		binary.BigEndian.PutUint64(block[offset:offset+8], entry.inodeNumber)
		block[offset+8] = byte(len(entry.name))
		copy(block[offset+9:], entry.name)
		if f.hasFileType() {
			block[offset+9+len(entry.name)] = entry.fileType
		}
		binary.BigEndian.PutUint16(block[offset+recordLength-2:offset+recordLength], uint16(offset))
		offset += recordLength
	}

	f.markFreeRun(block, offset, entriesEnd)
	return block
}

// markFreeRun writes an xfs_dir2_data_unused run covering [start, end).
func (f *fixtureImage) markFreeRun(block []byte, start, end int) {
	length := end - start
	if length < 4 {
		return
	}
	binary.BigEndian.PutUint16(block[start:start+2], 0xffff)
	binary.BigEndian.PutUint16(block[start+2:start+4], uint16(length))
	if length >= 6 {
		binary.BigEndian.PutUint16(block[end-2:end], uint16(start))
	}
}

// dotEntries returns the "." and ".." entries that head a real directory's
// first data block.
func dotEntries(self, parent uint64) []fixtureEntry {
	return []fixtureEntry{
		{name: ".", inodeNumber: self, fileType: DirEntryFileTypeDirectory},
		{name: "..", inodeNumber: parent, fileType: DirEntryFileTypeDirectory},
	}
}

// namedEntries generates count entries named prefix0..prefixN.
func namedEntries(prefix string, count int, firstInode uint64) []fixtureEntry {
	entries := make([]fixtureEntry, 0, count)
	for i := 0; i < count; i++ {
		entries = append(entries, fixtureEntry{
			name:        prefix + itoa(i),
			inodeNumber: firstInode + uint64(i),
			fileType:    DirEntryFileTypeRegularFile,
		})
	}
	return entries
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// entryNames extracts names for order-sensitive assertions.
func entryNames(entries []DirectoryEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names
}
