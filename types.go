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

	Size                     uint64
	NumberOfDataExtents      uint32
	NumberOfAttributesExtent uint16
	AttributesForkType       uint8
	DeviceIdentifier         uint32

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
}

// DirectoryRecord represents an active or deleted slot recovered from
// directory data structures.
type DirectoryRecord struct {
	Name         string
	InodeNumber  uint64
	IsDeleted    bool
	Offset       uint16
	RecordLength uint16
	IsCarved     bool
	Confidence   string
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
