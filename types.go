package libxfs

import "time"

type Superblock struct {
	BlockSize                uint32
	NumberOfBlocks           uint64
	JournalBlockNumber       uint64
	RootDirectoryInodeNumber uint64
	AllocationGroupSize      uint32
	NumberOfAllocationGroups uint32
	FormatVersion            uint8
	FeatureFlags             uint16
	SectorSize               uint16
	InodeSize                uint16
	DirectoryBlockSize       uint32
	VolumeLabel              [12]byte
	SecondaryFeatureFlags    uint32
	RelativeBlockNumberBits  uint8
	RelativeInodeNumberBits  uint8

	// v5-only feature words. These are zero on v4 filesystems, which do not
	// have the fields at all.
	FeaturesCompat         uint32
	FeaturesReadOnlyCompat uint32
	FeaturesIncompat       uint32
	FeaturesLogIncompat    uint32
}

// HasFeatureIncompat reports whether an incompatible feature bit is set.
func (s Superblock) HasFeatureIncompat(feature uint32) bool {
	return s.FeaturesIncompat&feature != 0
}

// HasBigTimestamps reports whether inode timestamps use the 64-bit "bigtime"
// encoding rather than the legacy 32-bit seconds/nanoseconds pair.
func (s Superblock) HasBigTimestamps() bool {
	return s.HasFeatureIncompat(FeatureIncompatBigTime)
}

// HasLargeExtentCounts reports whether inodes use the 64-bit extent counters
// introduced by the nrext64 feature.
func (s Superblock) HasLargeExtentCounts() bool {
	return s.HasFeatureIncompat(FeatureIncompatLargeExtentCounts)
}

// NeedsRepair reports whether the filesystem was marked as requiring repair.
// Such an image was left in an inconsistent state and its metadata should be
// treated with suspicion.
func (s Superblock) NeedsRepair() bool {
	return s.HasFeatureIncompat(FeatureIncompatNeedsRepair)
}

type InodeInformation struct {
	FormatVersion       uint32
	InodeBtreeRootBlock uint32
	InodeBtreeDepth     uint32
	LastAllocatedChunk  uint32
}

type Inode struct {
	FormatVersion uint8
	FileMode      uint16
	ForkType      uint8

	OwnerID       uint32
	GroupID       uint32
	NumberOfLinks uint32

	AccessTimeNS       int64
	ModificationTimeNS int64
	InodeChangeTimeNS  int64
	CreationTimeNS     int64

	Size uint64
	// NumberOfDataExtents is the data fork extent count, saturated to 32 bits.
	// On filesystems with the nrext64 feature the on-disk counter is 64 bits
	// wide; prefer DataExtentCount, which cannot overflow.
	NumberOfDataExtents uint32
	// NumberOfAttributesExtent is the attribute fork extent count, saturated
	// to 16 bits. Prefer AttributeExtentCount.
	NumberOfAttributesExtent uint16
	// DataExtentCount is the full-width data fork extent count.
	DataExtentCount uint64
	// AttributeExtentCount is the full-width attribute fork extent count.
	AttributeExtentCount uint32
	// HasBigTimestamps records whether this inode's timestamps were decoded
	// using the 64-bit bigtime encoding.
	HasBigTimestamps   bool
	AttributesForkType uint8
	DeviceIdentifier   uint32

	DataForkOffset       uint16
	DataForkSize         uint16
	AttributesForkOffset uint16
	AttributesForkSize   uint16

	InlineData  []byte
	DataExtents []Extent

	InlineAttributesData []byte
	AttributesExtents    []Extent
	Raw                  []byte
}

type Extent struct {
	LogicalBlockNumber  uint64
	PhysicalBlockNumber uint64
	NumberOfBlocks      uint32
	RangeFlags          uint32
}

type ExtendedAttribute struct {
	Name      string
	Namespace string
	Value     []byte
	Flags     uint8
}

type DirectoryEntry struct {
	Name        string
	InodeNumber uint64
	// FileType is the XFS directory entry file type (DirEntryFileType*).
	// It is DirEntryFileTypeUnknown on filesystems without the ftype feature.
	FileType uint8
}

// RecoveryConfidence labels how much trust a recovered directory record
// deserves. It is a string alias so that existing comparisons against plain
// string literals keep compiling.
type RecoveryConfidence = string

// Confidence levels applied to DirectoryRecord.Confidence.
const (
	ConfidenceLow    RecoveryConfidence = "low"
	ConfidenceMedium RecoveryConfidence = "medium"
	ConfidenceHigh   RecoveryConfidence = "high"
)

// DirectoryRecordKind distinguishes how a record was obtained. Confidence
// alone is not a safe gate: an active entry and a carved candidate can both be
// ConfidenceHigh. Switch on Kind, or use IsVerified/IsProbabilistic.
type DirectoryRecordKind = string

const (
	// RecordKindActive is an entry parsed from intact directory framing.
	RecordKindActive DirectoryRecordKind = "active"
	// RecordKindFreeSlot is an unused-space run. It marks reclaimed space and
	// carries no recovered name or inode number.
	RecordKindFreeSlot DirectoryRecordKind = "free_slot"
	// RecordKindCarved is a probabilistic candidate recovered from free space
	// by pattern matching. It may be stale, partial, or entirely spurious.
	RecordKindCarved DirectoryRecordKind = "carved"
)

// Evidence codes reported in DirectoryRecord.ConfidenceReasons.
const (
	ReasonIntactFraming    = "intact_framing"
	ReasonTagMatchesOffset = "tag_matches_offset"
	ReasonNamePrintable    = "name_printable"
	ReasonAlignedOffset    = "aligned_offset"
	ReasonFileTypeValid    = "ftype_valid"
	ReasonInodeAllocated   = "inode_allocated"
	ReasonInodeUnallocated = "inode_unallocated"
	// ReasonInodeUnaddressable marks a recovered inode number that cannot
	// address anything on this volume — strong evidence of a false match.
	ReasonInodeUnaddressable = "inode_unaddressable"
	ReasonInFreeSlot         = "in_free_slot"
)

// DirectoryRecord represents an active or deleted slot recovered from
// directory data structures.
type DirectoryRecord struct {
	Name         string
	InodeNumber  uint64
	IsDeleted    bool
	Offset       uint16
	RecordLength uint16
	IsCarved     bool
	Confidence   RecoveryConfidence

	// Kind describes how this record was obtained. Prefer it over the
	// IsDeleted/IsCarved pair when gating downstream decisions.
	Kind DirectoryRecordKind
	// FileType is the XFS directory entry file type (DirEntryFileType*).
	FileType uint8
	// BlockIndex is the directory-block index this record was found in.
	BlockIndex uint64
	// LogicalOffset is the absolute byte offset of the record within the
	// directory data stream. Offset is only meaningful within its block.
	LogicalOffset uint64
	// ConfidenceReasons lists the evidence codes behind Confidence.
	ConfidenceReasons []string
}

// IsVerified reports whether the record was parsed from intact directory
// framing and can be treated as fact.
func (r DirectoryRecord) IsVerified() bool {
	return r.Kind == RecordKindActive || (r.Kind == "" && !r.IsCarved && !r.IsDeleted)
}

// IsProbabilistic reports whether the record was carved heuristically and must
// be presented as a candidate rather than as fact.
func (r DirectoryRecord) IsProbabilistic() bool {
	return r.IsCarved
}

// FragmentationReport summarizes how a file's data is laid out across extents.
type FragmentationReport struct {
	InodeNumber                uint64
	Size                       uint64
	DataExtentCount            int
	AllocatedExtentCount       int
	SparseExtentCount          int
	PhysicalFragmentRuns       int
	HasLogicalHoles            bool
	HasPhysicalFragmentation   bool
	HasAnyFragmentationOrHoles bool
}

func (i *Inode) IsDirectory() bool {
	return (i.FileMode & 0xf000) == FileTypeDirectory
}

func (i *Inode) AccessTime() time.Time {
	return time.Unix(0, i.AccessTimeNS).UTC()
}

func (i *Inode) ModificationTime() time.Time {
	return time.Unix(0, i.ModificationTimeNS).UTC()
}

func (i *Inode) InodeChangeTime() time.Time {
	return time.Unix(0, i.InodeChangeTimeNS).UTC()
}

func (i *Inode) CreationTime() time.Time {
	if i.CreationTimeNS == 0 {
		return time.Time{}
	}
	return time.Unix(0, i.CreationTimeNS).UTC()
}
