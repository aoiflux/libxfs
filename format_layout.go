package libxfs

// On-disk structure layout.
//
// Every field offset and structure size this package reads is named here, in
// one place, grouped by the structure it belongs to. Nothing outside this file
// should contain a bare numeric offset.
//
// This is not cosmetic. Every high-severity bug found in this parser so far was
// a wrong offset or size hiding as a literal in the middle of a function: the
// v5 directory header read eight bytes early, timestamps decoded with the wrong
// layout, and extent counters read from their pre-nrext64 positions. Naming the
// offsets next to the struct they describe makes the next one reviewable.
//
// Field names follow the kernel's (fs/xfs/libxfs/xfs_format.h,
// xfs_da_format.h) so a reader can check them against the specification.

// Superblock (xfs_dsb) field offsets.
const (
	sbOffsetMagic                 = 0
	sbOffsetBlockSize             = 4
	sbOffsetDataBlocks            = 8
	sbOffsetLogStart              = 48
	sbOffsetRootInode             = 56
	sbOffsetAGBlocks              = 84
	sbOffsetAGCount               = 88
	sbOffsetVersionNumber         = 100
	sbOffsetSectorSize            = 102
	sbOffsetInodeSize             = 104
	sbOffsetInodesPerBlock        = 106
	sbOffsetFilesystemName        = 108
	sbOffsetInodesPerBlockLog     = 123
	sbOffsetAGBlocksLog           = 124
	sbOffsetDirectoryBlockLog     = 192
	sbOffsetSecondaryFeatureFlags = 204

	sbFilesystemNameLength = 12
)

// Superblock version word (sb_versionnum) layout.
const (
	sbVersionMask      = 0x000f
	sbFeatureFlagsMask = 0xfff0

	sbFormatVersion4 = 4
	sbFormatVersion5 = 5
)

// Feature bits accepted in the superblock version word. Anything outside this
// set changes the layout in a way this parser does not model.
const sbSupportedFeatureFlags = uint16(0x0010 | 0x0020 | 0x0080 | 0x0400 |
	0x0800 | 0x1000 | 0x2000 | 0x4000 | 0x8000)

// Geometry limits enforced when validating a superblock.
const (
	minSectorSize = 512
	maxSectorSize = 16384

	maxDirectoryBlockLog     = 32
	minAllocationGroupBlocks = 5
	maxAllocationGroupLog    = 31
	maxRelativeInodeBits     = 32
	inodeAddressBits         = 32
)

// Allocation group inode header (xfs_agi) field offsets.
const (
	agiOffsetMagic              = 0
	agiOffsetVersion            = 4
	agiOffsetInodeBtreeRoot     = 20
	agiOffsetInodeBtreeDepth    = 24
	agiOffsetLastAllocatedChunk = 32

	agiSizeV4 = 296
	agiSizeV5 = 512

	// agiSectorIndex is the sector within an allocation group that holds the
	// AGI: superblock, AGF, then AGI.
	agiSectorIndex = 2
)

// Inode (xfs_dinode) field offsets.
const (
	inodeOffsetMagic = 0
	inodeOffsetMode  = 2
	// inodeOffsetVersion holds di_version: 1 and 2 are the v4 layouts, 3 is v5.
	inodeOffsetVersion = 4
	inodeOffsetFormat  = 5
	// inodeOffsetLinkCountV1 is di_onlink, used only by version 1 inodes.
	inodeOffsetLinkCountV1 = 6
	inodeOffsetUserID      = 8
	inodeOffsetGroupID     = 12
	inodeOffsetLinkCount   = 16
	// inodeOffsetBigExtentCount is di_big_nextents, present only with the
	// nrext64 feature, which repurposes the v3 padding here.
	inodeOffsetBigExtentCount     = 24
	inodeOffsetAccessTime         = 32
	inodeOffsetModificationTime   = 40
	inodeOffsetChangeTime         = 48
	inodeOffsetSize               = 56
	inodeOffsetBlockCount         = 64
	inodeOffsetExtentSize         = 72
	inodeOffsetExtentCount        = 76
	inodeOffsetAttributeExtents   = 80
	inodeOffsetAttributeForkShift = 82
	inodeOffsetAttributeFormat    = 83
	inodeOffsetCreationTime       = 144
)

// Inode header sizes. The fork area begins immediately after the header.
const (
	inodeHeaderSizeV2 = 100
	inodeHeaderSizeV3 = 176

	inodeVersion1 = 1
	inodeVersion2 = 2
	inodeVersion3 = 3

	// inodeAttributeForkShiftUnit is the multiplier applied to di_forkoff,
	// which records the attribute fork offset in eight-byte units.
	inodeAttributeForkShiftUnit = 8

	deviceIdentifierSize = 4
)

// Inode b-tree (xfs_inobt) record layout.
const (
	inobtRecordSize             = 16
	inobtRecordOffsetStartInode = 0
	inobtRecordOffsetFreeCount  = 4
	inobtRecordOffsetFreeMask   = 8

	// inobtInodesPerChunk is the number of inodes described by one record.
	inobtInodesPerChunk = 64

	inobtKeySize     = 4
	inobtPointerSize = 4
)

// Short-form b-tree block headers (xfs_btree_sblock / xfs_btree_lblock).
//
// The v5 variants add a CRC, block number, LSN, UUID and owner. Sizes differ
// by whether sibling pointers are 32 or 64 bits wide.
const (
	btreeHeaderSizeShortV4 = 16
	btreeHeaderSizeShortV5 = 56
	btreeHeaderSizeLongV4  = 24
	btreeHeaderSizeLongV5  = 72

	btreeOffsetMagic       = 0
	btreeOffsetLevel       = 4
	btreeOffsetRecordCount = 6

	btreePointerSizeShort = 4
	btreePointerSizeLong  = 8
)

// B-tree block magics.
const (
	btreeMagicLength   = 4
	inodeBtreeMagicV4  = "IABT"
	inodeBtreeMagicV5  = "IAB3"
	extentBtreeMagicV4 = "BMAP"
	extentBtreeMagicV5 = "BMA3"
)

// Extent record (xfs_bmbt_rec) bit layout.
//
// A record is two big-endian 64-bit words:
//
//	upper: flag(1) | logical block(54) | high bits of start block(9)
//	lower: low bits of start block(43) | block count(21)
const (
	extentRecordSize = 16

	extentBlockCountBits  = 21
	extentBlockCountMask  = (1 << extentBlockCountBits) - 1
	extentStartLowBits    = 43
	extentStartLowMask    = (uint64(1) << extentStartLowBits) - 1
	extentStartHighBits   = 9
	extentStartHighMask   = (uint64(1) << extentStartHighBits) - 1
	extentLogicalBlockPos = 9
	extentLogicalBits     = 54
	extentLogicalMask     = (uint64(1) << extentLogicalBits) - 1
	extentUnwrittenShift  = 63
)

// Extended attribute structures.
const (
	// Short-form attribute header: count(1) + padding(3).
	xattrShortFormHeaderSize  = 3
	xattrShortFormEntryHeader = 3

	// Attribute block headers. The v5 variants carry the CRC block info.
	xattrBlockHeaderSizeV4 = 32
	xattrBlockHeaderSizeV5 = 48

	xattrLeafEntrySize = 8
	xattrBranchKeySize = 8
)

// Attribute entry flags (XFS_ATTR_*).
const (
	xattrFlagLocal      uint8 = 0x01
	xattrFlagRoot       uint8 = 0x02
	xattrFlagSecure     uint8 = 0x04
	xattrFlagIncomplete uint8 = 0x80

	// xattrNamespaceMask isolates the namespace bits, discarding the local and
	// incomplete flags which describe storage rather than namespace.
	xattrNamespaceMask uint8 = 0x7e
)

// Extended attribute namespace names, as Linux presents them. Downstream tools
// match on these strings, so they are part of the library's contract.
const (
	XattrNamespaceUser     = "user"
	XattrNamespaceTrusted  = "trusted"
	XattrNamespaceSecurity = "security"
)

// File mode type bits (S_IFMT).
const fileModeTypeMask = 0xf000

// Attribute leaf/branch block magics and sizes not covered above.
const (
	xattrLeafMagicV4 uint16 = 0xfbee
	xattrLeafMagicV5 uint16 = 0x3bee

	// xattr leaf header follows the block info header.
	xattrLeafHeaderSizeV4 = 20
	xattrLeafHeaderSizeV5 = 24

	// xattrRemoteValueMagic heads a v5 remote attribute value block.
	xattrRemoteValueMagic = "XARM"
)

// namespaceSeparator joins an attribute namespace to its name, as Linux
// presents fully-qualified attribute names.
const namespaceSeparator = "."

// Severity levels reported in ReportAnomaly.Severity.
const (
	SeverityInfo    = "info"
	SeverityLow     = "low"
	SeverityMedium  = "medium"
	SeverityWarning = "warning"
	SeverityHigh    = "high"
	SeverityError   = "error"
)

// Inode type labels reported in InodeForensicReport.Type.
const (
	InodeTypeFile      = "file"
	InodeTypeDirectory = "directory"
)

// FilesystemTypeName is the filesystem label reported by VolumeIntegrityReport.
const FilesystemTypeName = "xfs"

// Short-form directory header (xfs_dir2_sf_hdr) layout.
//
// count(1) + i8count(1) + parent inode (4 or 8 bytes, chosen by i8count).
const (
	shortFormOffsetCount   = 0
	shortFormOffsetI8Count = 1
	shortFormOffsetParent  = 2

	shortFormInodeSize32 = 4
	shortFormInodeSize64 = 8

	shortFormHeaderSize32 = shortFormOffsetParent + shortFormInodeSize32
	shortFormHeaderSize64 = shortFormOffsetParent + shortFormInodeSize64
)

// Extent b-tree key/pointer pair size in an interior node.
const extentBtreeKeyPointerPairSize = 16

// Printable ASCII bounds used when judging whether carved bytes look like a
// filename.
const (
	printableASCIIMin = 0x20
	printableASCIIMax = 0x7e
)

// Superblock checksum (v5 only).
const (
	sbOffsetChecksum = 224
	checksumSize     = 4
)

// reportInitialCapacity presizes report slices and maps for a typical volume.
const reportInitialCapacity = 64
