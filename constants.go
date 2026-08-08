package libxfs

const (
	superblockSize      = 512
	inodeInformationLen = 512

	xfsSuperblockMagic = "XFSB"
	xfsInodeMagic      = "IN"
	xfsAGIMagic        = "XAGI"
)

const (
	minBlockSize = 512
	maxBlockSize = 65536

	minInodeSize = 256
	maxInodeSize = 2048
)

const (
	FileTypeFIFO            uint16 = 0x1000
	FileTypeCharacterDevice uint16 = 0x2000
	FileTypeDirectory       uint16 = 0x4000
	FileTypeBlockDevice     uint16 = 0x6000
	FileTypeRegularFile     uint16 = 0x8000
	FileTypeSymbolicLink    uint16 = 0xa000
	FileTypeSocket          uint16 = 0xc000
)

const (
	ForkTypeDevice     uint8 = 0
	ForkTypeInlineData uint8 = 1
	ForkTypeExtents    uint8 = 2
	ForkTypeBtree      uint8 = 3
)

// Incompatible feature bits from the v5 superblock (sb_features_incompat).
//
// A filesystem carrying an incompatible bit cannot be interpreted correctly by
// an implementation that does not understand it. BigTime and LargeExtentCounts
// both change the on-disk inode layout.
const (
	// FeatureIncompatFileType marks directory entries as carrying an ftype byte.
	FeatureIncompatFileType uint32 = 1 << 0
	// FeatureIncompatSparseInodes marks sparse inode chunk allocation.
	FeatureIncompatSparseInodes uint32 = 1 << 1
	// FeatureIncompatMetaUUID marks metadata stamped with a separate UUID.
	FeatureIncompatMetaUUID uint32 = 1 << 2
	// FeatureIncompatBigTime marks 64-bit nanosecond inode timestamps.
	FeatureIncompatBigTime uint32 = 1 << 3
	// FeatureIncompatNeedsRepair marks a filesystem left needing repair.
	FeatureIncompatNeedsRepair uint32 = 1 << 4
	// FeatureIncompatLargeExtentCounts marks 64-bit inode extent counters
	// (the nrext64 feature).
	FeatureIncompatLargeExtentCounts uint32 = 1 << 5
)

// knownFeaturesIncompat is the set of incompatible features this parser
// understands well enough to read correctly.
const knownFeaturesIncompat = FeatureIncompatFileType |
	FeatureIncompatSparseInodes |
	FeatureIncompatMetaUUID |
	FeatureIncompatBigTime |
	FeatureIncompatNeedsRepair |
	FeatureIncompatLargeExtentCounts

// Superblock offsets for the v5-only feature words.
const (
	superblockFeaturesCompatOffset      = 208
	superblockFeaturesReadOnlyOffset    = 212
	superblockFeaturesIncompatOffset    = 216
	superblockFeaturesLogIncompatOffset = 220
)

// xfsBigTimeEpochOffsetSeconds is the offset between the bigtime epoch
// (1901-12-13 20:45:52 UTC) and the Unix epoch. The kernel expresses this as
// -(int64)S32_MIN.
const xfsBigTimeEpochOffsetSeconds = int64(2147483648)

const maxBtreeRecursionDepth = 256

// Extent range flags reported in Extent.RangeFlags.
const (
	// ExtentFlagSparse marks a range that reads back as zeros. It covers both
	// unmapped holes and preallocated-but-unwritten extents, because from a
	// reader's point of view they are the same thing.
	ExtentFlagSparse uint32 = 0x00000001
	// ExtentFlagUnwritten marks a range that is allocated on disk but has
	// never been written, as produced by fallocate. It is always accompanied
	// by ExtentFlagSparse, since it too reads as zeros, but unlike a hole it
	// has a real PhysicalBlockNumber and occupies space.
	//
	// The distinction matters forensically: a hole says nothing was ever
	// stored there, while an unwritten extent names blocks that were reserved,
	// and whose previous contents may still be on the medium.
	ExtentFlagUnwritten uint32 = 0x00000002
)
