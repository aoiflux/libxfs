package libxfs

// On-disk directory format constants.
//
// Values mirror the XFS on-disk format as defined by
// fs/xfs/libxfs/xfs_da_format.h in the Linux kernel and xfsprogs.

// Directory block magic numbers stored as big-endian ASCII in the first four
// bytes of a directory block.
const (
	dirBlockMagicV4 = "XD2B" // XFS_DIR2_BLOCK_MAGIC 0x58443242
	dirDataMagicV4  = "XD2D" // XFS_DIR2_DATA_MAGIC  0x58443244
	dirFreeMagicV4  = "XD2F" // XFS_DIR2_FREE_MAGIC  0x58443246

	dirBlockMagicV5 = "XDB3" // XFS_DIR3_BLOCK_MAGIC 0x58444233
	dirDataMagicV5  = "XDD3" // XFS_DIR3_DATA_MAGIC  0x58444433
	dirFreeMagicV5  = "XDF3" // XFS_DIR3_FREE_MAGIC  0x58444633
)

// Leaf and node blocks carry a 16-bit magic inside xfs_da_blkinfo, at offset 8
// of the block, rather than a four byte ASCII magic.
const (
	dirLeaf1MagicV4 uint16 = 0xd2f1 // XFS_DIR2_LEAF1_MAGIC
	dirLeafNMagicV4 uint16 = 0xd2ff // XFS_DIR2_LEAFN_MAGIC
	dirLeaf1MagicV5 uint16 = 0x3df1 // XFS_DIR3_LEAF1_MAGIC
	dirLeafNMagicV5 uint16 = 0x3dff // XFS_DIR3_LEAFN_MAGIC
	daNodeMagicV4   uint16 = 0xfebe // XFS_DA_NODE_MAGIC
	daNodeMagicV5   uint16 = 0x3ebe // XFS_DA3_NODE_MAGIC
)

const daBlockInfoMagicOffset = 8

// Directory data block header sizes.
//
// v4 (xfs_dir2_data_hdr):  magic(4) + bestfree[3](3*4) = 16
// v5 (xfs_dir3_data_hdr):  xfs_dir3_blk_hdr(48) + best_free[3](3*4) + pad(4) = 64
//
// The v5 header is 64 bytes, not 56: xfs_dir3_blk_hdr is magic(4) + crc(4) +
// blkno(8) + lsn(8) + uuid(16) + owner(8) = 48. 56 is the size of
// xfs_da3_blkinfo, which heads leaf and node blocks, not data blocks.
const (
	dir2DataHeaderSize = 16
	dir3DataHeaderSize = 64
)

// Directory entry framing.
//
// An active entry (xfs_dir2_data_entry) is inumber(8) + namelen(1) + name +
// optional ftype(1) + tag(2), rounded up to an 8 byte boundary. A free run
// (xfs_dir2_data_unused) is freetag(2) + length(2) + ... + tag(2).
const (
	dirDataAlign          = 8
	dirDataFreeTag        = 0xffff // XFS_DIR2_DATA_FREE_TAG
	dirEntryOverhead      = 11     // inumber(8) + namelen(1) + tag(2)
	dirUnusedHeaderSize   = 4      // freetag(2) + length(2)
	dirBlockTailSize      = 8      // xfs_dir2_block_tail: count(4) + stale(4)
	dirLeafEntrySize      = 8      // xfs_dir2_leaf_entry: hashval(4) + address(4)
	maxDirectoryNameBytes = 255
)

// maxAnomaliesPerDirectoryBlock bounds how many framing errors a single block
// may report in best-effort mode. Past this point the block is noise, and
// listing every misaligned byte helps nobody.
const maxAnomaliesPerDirectoryBlock = 64

// Directory logical address space.
//
// XFS divides a directory's logical byte space into fixed regions:
//
//	XFS_DIR2_SPACE_SIZE = 1ULL << (32 + XFS_DIR2_DATA_ALIGN_LOG) = 32 GiB
//
// Data blocks occupy space 0, leaf blocks space 1, free blocks space 2.
// xfs_dir2_grow_inode only advances di_size for the data space, so di_size is
// always one past the last data block and never reaches the leaf offset.
const (
	dirSpaceSize       = uint64(1) << 35
	dirDataSpaceOffset = 0 * dirSpaceSize
	dirLeafSpaceOffset = 1 * dirSpaceSize
	dirFreeSpaceOffset = 2 * dirSpaceSize
)

// Directory entry file types (XFS_DIR3_FT_*).
//
// These are stored in the optional ftype byte of a directory entry when the
// filesystem has the ftype feature enabled, and describe the target inode
// without requiring it to be read.
const (
	DirEntryFileTypeUnknown         uint8 = 0
	DirEntryFileTypeRegularFile     uint8 = 1
	DirEntryFileTypeDirectory       uint8 = 2
	DirEntryFileTypeCharacterDevice uint8 = 3
	DirEntryFileTypeBlockDevice     uint8 = 4
	DirEntryFileTypeFIFO            uint8 = 5
	DirEntryFileTypeSocket          uint8 = 6
	DirEntryFileTypeSymbolicLink    uint8 = 7
	DirEntryFileTypeWhiteout        uint8 = 8

	dirEntryFileTypeMax uint8 = 9 // XFS_DIR3_FT_MAX
)

// DirEntryFileTypeName returns a human readable name for an XFS directory
// entry file type value.
func DirEntryFileTypeName(fileType uint8) string {
	switch fileType {
	case DirEntryFileTypeRegularFile:
		return "regular_file"
	case DirEntryFileTypeDirectory:
		return "directory"
	case DirEntryFileTypeCharacterDevice:
		return "character_device"
	case DirEntryFileTypeBlockDevice:
		return "block_device"
	case DirEntryFileTypeFIFO:
		return "fifo"
	case DirEntryFileTypeSocket:
		return "socket"
	case DirEntryFileTypeSymbolicLink:
		return "symbolic_link"
	case DirEntryFileTypeWhiteout:
		return "whiteout"
	default:
		return "unknown"
	}
}

// directoryBlockKind classifies a directory block by its on-disk magic.
type directoryBlockKind int

const (
	dirBlockKindUnknown directoryBlockKind = iota
	dirBlockKindEmpty
	dirBlockKindData  // XD2D / XDD3: entries only
	dirBlockKindBlock // XD2B / XDB3: entries plus trailing leaf array and tail
	dirBlockKindLeaf
	dirBlockKindNode
	dirBlockKindFree
)

func (k directoryBlockKind) String() string {
	switch k {
	case dirBlockKindEmpty:
		return "empty"
	case dirBlockKindData:
		return "data"
	case dirBlockKindBlock:
		return "block"
	case dirBlockKindLeaf:
		return "leaf"
	case dirBlockKindNode:
		return "node"
	case dirBlockKindFree:
		return "free"
	default:
		return "unknown"
	}
}

// classifyDirectoryBlock identifies a directory block from its own magic and
// returns the size of its header.
//
// Classification is driven by the block contents rather than by the superblock
// format version so that a damaged or mixed image is still described
// accurately. headerSize is only meaningful for data and block kinds.
func classifyDirectoryBlock(block []byte) (kind directoryBlockKind, headerSize int) {
	if len(block) < 4 {
		return dirBlockKindUnknown, 0
	}
	if isZeroBytes(block) {
		return dirBlockKindEmpty, 0
	}

	switch string(block[0:4]) {
	case dirDataMagicV4:
		return dirBlockKindData, dir2DataHeaderSize
	case dirDataMagicV5:
		return dirBlockKindData, dir3DataHeaderSize
	case dirBlockMagicV4:
		return dirBlockKindBlock, dir2DataHeaderSize
	case dirBlockMagicV5:
		return dirBlockKindBlock, dir3DataHeaderSize
	case dirFreeMagicV4, dirFreeMagicV5:
		return dirBlockKindFree, 0
	}

	if len(block) >= daBlockInfoMagicOffset+2 {
		magic, ok := readUint16BE(block, daBlockInfoMagicOffset)
		if ok {
			switch magic {
			case dirLeaf1MagicV4, dirLeafNMagicV4, dirLeaf1MagicV5, dirLeafNMagicV5:
				return dirBlockKindLeaf, 0
			case daNodeMagicV4, daNodeMagicV5:
				return dirBlockKindNode, 0
			}
		}
	}

	return dirBlockKindUnknown, 0
}

func isZeroBytes(data []byte) bool {
	for _, b := range data {
		if b != 0 {
			return false
		}
	}
	return true
}
