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

const maxBtreeRecursionDepth = 256

const (
	ExtentFlagSparse uint32 = 0x00000001
)
