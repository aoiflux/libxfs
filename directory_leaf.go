package libxfs

import (
	"fmt"
	"sort"
)

// Leaf and node block parsing for directories.
//
// Entries themselves always live in the data-block region, so listing a
// directory never needs this file (see the walker in directory_scan.go). What
// the leaf space provides is the hash index: a name-ordered map from name hash
// to entry location, which allows a single lookup without reading every data
// block, and which can be cross-checked against the data blocks to detect
// tampering.

// Header sizes for da (directory/attribute) blocks.
//
// v4: xfs_da_blkinfo    = forw(4) + back(4) + magic(2) + pad(2)          = 12
// v5: xfs_da3_blkinfo   = xfs_da_blkinfo(12) + crc(4) + blkno(8) +
//
//	lsn(8) + uuid(16) + owner(8)                                       = 56
//
// xfs_dir2_leaf_hdr = blkinfo + count(2) + stale(2)
// xfs_dir3_leaf_hdr = da3_blkinfo + count(2) + stale(2) + pad(4)
// xfs_da_node_hdr   = blkinfo + count(2) + level(2)
// xfs_da3_node_hdr  = da3_blkinfo + count(2) + level(2) + pad32(4)
const (
	daBlockInfoSizeV4 = 12
	daBlockInfoSizeV5 = 56

	dirLeafHeaderSizeV4 = daBlockInfoSizeV4 + 4 // 16
	dirLeafHeaderSizeV5 = daBlockInfoSizeV5 + 8 // 64
	daNodeHeaderSizeV4  = daBlockInfoSizeV4 + 4 // 16
	daNodeHeaderSizeV5  = daBlockInfoSizeV5 + 8 // 64

	dirLeafEntryPairSize = 8 // hashval(4) + address(4)
	daNodeEntryPairSize  = 8 // hashval(4) + before(4)
)

// maxDirectoryLeafBlocks bounds a node-tree walk on a damaged image.
const maxDirectoryLeafBlocks = 4096

// directoryLeafEntry is one hash-index entry: a name hash and the location of
// the matching entry, expressed as an xfs_dir2_dataptr_t.
type directoryLeafEntry struct {
	Hash uint32
	// Address is the entry's byte offset within the data space divided by
	// eight. Zero means the slot is unused.
	Address uint32
}

// xfsDirHashName is xfs_da_hashname: the rolling hash XFS uses to order
// directory entries in the leaf index.
func xfsDirHashName(name []byte) uint32 {
	var hash uint32

	index := 0
	for ; len(name)-index >= 4; index += 4 {
		hash = uint32(name[index])<<21 ^
			uint32(name[index+1])<<14 ^
			uint32(name[index+2])<<7 ^
			uint32(name[index+3]) ^
			rotateLeft32(hash, 7*4)
	}

	switch len(name) - index {
	case 3:
		return uint32(name[index])<<14 ^
			uint32(name[index+1])<<7 ^
			uint32(name[index+2]) ^
			rotateLeft32(hash, 7*3)
	case 2:
		return uint32(name[index])<<7 ^
			uint32(name[index+1]) ^
			rotateLeft32(hash, 7*2)
	case 1:
		return uint32(name[index]) ^ rotateLeft32(hash, 7*1)
	default:
		return hash
	}
}

func rotateLeft32(value uint32, count uint) uint32 {
	return value<<count | value>>(32-count)
}

// readDirectoryLogicalBlock reads size bytes at a logical byte offset in the
// directory's address space.
//
// The walker in directory_scan.go cannot be reused here: it bounds reads by
// di_size, which only covers the data space, whereas leaf and node blocks live
// above the 32 GiB boundary. An unmapped or sparse range is an error rather
// than a hole, because the leaf space is never sparse on a healthy directory.
func (v *Volume) readDirectoryLogicalBlock(inode *Inode, logicalByteOffset uint64, size uint64) ([]byte, error) {
	blockSize := uint64(v.ioh.blockSize)
	if blockSize == 0 {
		return nil, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}
	if size == 0 || size%blockSize != 0 {
		return nil, wrapParseError(int64(size), "directory_block_size", ErrInvalidSuperblock)
	}
	if logicalByteOffset%blockSize != 0 {
		return nil, wrapParseError(int64(logicalByteOffset), "directory_logical_offset", ErrInvalidInode)
	}

	firstBlock := logicalByteOffset / blockSize
	blockCount := size / blockSize

	// Verify the whole range is mapped before allocating: the common case is a
	// directory with no hash index at all, and allocating a block buffer only
	// to discard it made every lookup on such a directory measurably slower.
	for i := uint64(0); i < blockCount; i++ {
		logicalBlock := firstBlock + i
		extent, _ := findExtentForBlock(inode.DataExtents, logicalBlock)
		if extent == nil {
			return nil, fmt.Errorf("%w: directory logical block %d is not mapped",
				ErrUnsupportedDirFormat, logicalBlock)
		}
		if extent.RangeFlags&ExtentFlagSparse != 0 {
			return nil, fmt.Errorf("%w: directory logical block %d is sparse",
				ErrUnsupportedDirFormat, logicalBlock)
		}
	}

	buffer := make([]byte, size)
	for i := uint64(0); i < blockCount; i++ {
		logicalBlock := firstBlock + i
		extent, _ := findExtentForBlock(inode.DataExtents, logicalBlock)

		physicalBlock := extent.PhysicalBlockNumber + (logicalBlock - extent.LogicalBlockNumber)
		diskOffset := int64(physicalBlock * blockSize)
		if err := v.readAt(buffer[i*blockSize:(i+1)*blockSize], diskOffset); err != nil {
			return nil, wrapIOError("read", diskOffset, int(blockSize), err)
		}
	}
	return buffer, nil
}

// parseDirectoryLeafBlock decodes the hash index entries of a leaf block.
func parseDirectoryLeafBlock(block []byte) ([]directoryLeafEntry, error) {
	magic, ok := readUint16BE(block, daBlockInfoMagicOffset)
	if !ok {
		return nil, wrapParseError(daBlockInfoMagicOffset, "directory_leaf_magic", ErrInvalidInode)
	}

	headerSize := 0
	switch magic {
	case dirLeaf1MagicV4, dirLeafNMagicV4:
		headerSize = dirLeafHeaderSizeV4
	case dirLeaf1MagicV5, dirLeafNMagicV5:
		headerSize = dirLeafHeaderSizeV5
	default:
		return nil, fmt.Errorf("%w: not a directory leaf block (magic 0x%04x)",
			ErrUnsupportedDirFormat, magic)
	}
	if len(block) < headerSize {
		return nil, wrapParseError(0, "directory_leaf_header", ErrInvalidInode)
	}

	// count and stale sit immediately after the block info header.
	count, ok := readUint16BE(block, headerSize-4)
	if !ok {
		return nil, wrapParseError(int64(headerSize-4), "directory_leaf_count", ErrInvalidInode)
	}

	available := (len(block) - headerSize) / dirLeafEntryPairSize
	if int(count) > available {
		return nil, wrapParseError(int64(headerSize-4), "directory_leaf_count", ErrInvalidInode)
	}

	entries := make([]directoryLeafEntry, 0, count)
	for i := 0; i < int(count); i++ {
		offset := headerSize + i*dirLeafEntryPairSize
		hash, okHash := readUint32BE(block, offset)
		address, okAddress := readUint32BE(block, offset+4)
		if !okHash || !okAddress {
			return nil, wrapParseError(int64(offset), "directory_leaf_entry", ErrInvalidInode)
		}
		entries = append(entries, directoryLeafEntry{Hash: hash, Address: address})
	}
	return entries, nil
}

// parseDirectoryNodeBlock decodes an interior da node block, returning its
// child pointers. Child values are xfs_dablk_t: logical filesystem-block
// numbers within the directory.
func parseDirectoryNodeBlock(block []byte) (level uint16, children []uint32, err error) {
	magic, ok := readUint16BE(block, daBlockInfoMagicOffset)
	if !ok {
		return 0, nil, wrapParseError(daBlockInfoMagicOffset, "directory_node_magic", ErrInvalidInode)
	}

	headerSize := 0
	switch magic {
	case daNodeMagicV4:
		headerSize = daNodeHeaderSizeV4
	case daNodeMagicV5:
		headerSize = daNodeHeaderSizeV5
	default:
		return 0, nil, fmt.Errorf("%w: not a directory node block (magic 0x%04x)",
			ErrUnsupportedDirFormat, magic)
	}
	if len(block) < headerSize {
		return 0, nil, wrapParseError(0, "directory_node_header", ErrInvalidInode)
	}

	// count and level follow the block info header.
	countOffset := daBlockInfoSizeV4
	if magic == daNodeMagicV5 {
		countOffset = daBlockInfoSizeV5
	}
	count, okCount := readUint16BE(block, countOffset)
	level, okLevel := readUint16BE(block, countOffset+2)
	if !okCount || !okLevel {
		return 0, nil, wrapParseError(int64(countOffset), "directory_node_count", ErrInvalidInode)
	}

	available := (len(block) - headerSize) / daNodeEntryPairSize
	if int(count) > available {
		return 0, nil, wrapParseError(int64(countOffset), "directory_node_count", ErrInvalidInode)
	}

	children = make([]uint32, 0, count)
	for i := 0; i < int(count); i++ {
		offset := headerSize + i*daNodeEntryPairSize
		before, ok := readUint32BE(block, offset+4)
		if !ok {
			return 0, nil, wrapParseError(int64(offset+4), "directory_node_entry", ErrInvalidInode)
		}
		children = append(children, before)
	}
	return level, children, nil
}

// collectDirectoryLeafEntries gathers the whole hash index for a directory.
//
// The leaf space either holds a single leaf block (leaf format) or a da node
// tree whose leaves are leafn blocks (node format).
func (v *Volume) collectDirectoryLeafEntries(inode *Inode) ([]directoryLeafEntry, error) {
	blockSize := uint64(v.ioh.blockSize)
	if blockSize == 0 {
		return nil, wrapParseError(0, "block_size", ErrInvalidSuperblock)
	}

	// Short-form and single-block directories have no leaf space. Detecting
	// that from the extent map costs nothing and keeps the indexless case,
	// which is the overwhelming majority, off the slower path entirely.
	if extent, _ := findExtentForBlock(inode.DataExtents, dirLeafSpaceOffset/blockSize); extent == nil {
		return nil, fmt.Errorf("%w: directory has no hash index", ErrUnsupportedDirFormat)
	}

	directoryBlockSize, err := v.validateDirectoryBlockSize()
	if err != nil {
		return nil, err
	}

	root, err := v.readDirectoryLogicalBlock(inode, dirLeafSpaceOffset, directoryBlockSize)
	if err != nil {
		return nil, err
	}

	magic, _ := readUint16BE(root, daBlockInfoMagicOffset)
	switch magic {
	case dirLeaf1MagicV4, dirLeaf1MagicV5, dirLeafNMagicV4, dirLeafNMagicV5:
		return parseDirectoryLeafBlock(root)
	case daNodeMagicV4, daNodeMagicV5:
	default:
		return nil, fmt.Errorf("%w: unexpected block at the directory leaf offset (magic 0x%04x)",
			ErrUnsupportedDirFormat, magic)
	}

	// Node format: descend to every leaf, breadth first.
	var entries []directoryLeafEntry
	pending := [][]byte{root}
	visited := map[uint32]bool{}
	blocksRead := 0

	for len(pending) > 0 {
		block := pending[0]
		pending = pending[1:]

		_, children, err := parseDirectoryNodeBlock(block)
		if err != nil {
			return nil, err
		}

		for _, child := range children {
			if visited[child] {
				continue
			}
			visited[child] = true

			blocksRead++
			if blocksRead > maxDirectoryLeafBlocks {
				return nil, fmt.Errorf("%w: directory hash index exceeds %d blocks",
					ErrUnsupportedDirFormat, maxDirectoryLeafBlocks)
			}

			childBlock, err := v.readDirectoryLogicalBlock(inode, uint64(child)*blockSize, directoryBlockSize)
			if err != nil {
				return nil, err
			}

			childMagic, _ := readUint16BE(childBlock, daBlockInfoMagicOffset)
			switch childMagic {
			case daNodeMagicV4, daNodeMagicV5:
				pending = append(pending, childBlock)
			default:
				leafEntries, err := parseDirectoryLeafBlock(childBlock)
				if err != nil {
					return nil, err
				}
				entries = append(entries, leafEntries...)
			}
		}
	}
	return entries, nil
}

// directoryAddressToOffset converts an xfs_dir2_dataptr_t into a byte offset
// within the directory's data space.
func directoryAddressToOffset(address uint32) uint64 {
	return uint64(address) * dirDataAlign
}

// readDirectoryEntryAt decodes the entry at a byte offset in the data space.
func (v *Volume) readDirectoryEntryAt(inode *Inode, byteOffset uint64, hasFileType bool) (DirectoryEntry, error) {
	directoryBlockSize, err := v.validateDirectoryBlockSize()
	if err != nil {
		return DirectoryEntry{}, err
	}
	if byteOffset >= inode.Size {
		return DirectoryEntry{}, wrapParseError(int64(byteOffset), "directory_entry_address", ErrInvalidInode)
	}

	blockIndex := byteOffset / directoryBlockSize
	inBlock := int(byteOffset % directoryBlockSize)

	block, err := v.readDirectoryBlock(inode, blockIndex)
	if err != nil {
		return DirectoryEntry{}, err
	}
	if inBlock+dirEntryOverhead > len(block) {
		return DirectoryEntry{}, wrapParseError(int64(byteOffset), "directory_entry_address", ErrInvalidInode)
	}

	inodeNumber, ok := readUint64BE(block, inBlock)
	if !ok {
		return DirectoryEntry{}, wrapParseError(int64(byteOffset), "directory_entry_inode", ErrInvalidInode)
	}
	nameSize := int(block[inBlock+8])
	if nameSize == 0 || inodeNumber == 0 {
		return DirectoryEntry{}, wrapParseError(int64(byteOffset), "directory_entry_identity", ErrInvalidInode)
	}

	nameStart := inBlock + 9
	nameEnd := nameStart + nameSize
	if nameEnd > len(block) {
		return DirectoryEntry{}, wrapParseError(int64(byteOffset), "directory_entry_name", ErrInvalidInode)
	}

	fileType := DirEntryFileTypeUnknown
	if hasFileType {
		if nameEnd >= len(block) {
			return DirectoryEntry{}, wrapParseError(int64(byteOffset), "directory_entry_type", ErrInvalidInode)
		}
		fileType = block[nameEnd]
	}

	return DirectoryEntry{
		Name:        string(block[nameStart:nameEnd]),
		InodeNumber: inodeNumber,
		FileType:    fileType,
	}, nil
}

// lookupDirectoryEntryByHash resolves one name through the hash index.
//
// It returns found=false when the directory has no usable index, so the caller
// can fall back to a linear walk. An optimisation must never reduce recall on a
// damaged image.
func (v *Volume) lookupDirectoryEntryByHash(inode *Inode, name string) (DirectoryEntry, bool) {
	if inode.ForkType != ForkTypeExtents && inode.ForkType != ForkTypeBtree {
		return DirectoryEntry{}, false
	}

	entries, err := v.collectDirectoryLeafEntries(inode)
	if err != nil || len(entries) == 0 {
		return DirectoryEntry{}, false
	}

	target := xfsDirHashName([]byte(name))
	hasFileType := directoryHasFileType(v.ioh.formatVersion, v.ioh.secondaryFeatureFlags)

	// Leaf entries are ordered by hash, so a binary search finds the first
	// candidate; collisions are contiguous from there.
	start := sort.Search(len(entries), func(i int) bool {
		return entries[i].Hash >= target
	})

	for i := start; i < len(entries) && entries[i].Hash == target; i++ {
		if entries[i].Address == 0 {
			continue
		}
		entry, err := v.readDirectoryEntryAt(inode, directoryAddressToOffset(entries[i].Address), hasFileType)
		if err != nil {
			// A broken index entry means the index cannot be trusted.
			return DirectoryEntry{}, false
		}
		if entry.Name == name {
			return entry, true
		}
	}

	// The index was readable and simply does not contain the name. That is a
	// negative answer only if the index is complete, which callers verify with
	// VerifyDirectoryIndex; treat it as "unknown" and let the walk decide.
	return DirectoryEntry{}, false
}

// DirectoryIndexReport compares a directory's hash index against the entries
// actually present in its data blocks.
//
// The two structures are maintained together by the kernel, so any divergence
// means the directory was modified by something that did not maintain both —
// a tampering indicator that no other check in this package provides.
type DirectoryIndexReport struct {
	InodeNumber uint64 `json:"inode_number"`
	// HasIndex is false for short-form and single-block directories, which
	// have no separate hash index to check.
	HasIndex bool `json:"has_index"`
	// IndexedEntries counts usable entries in the hash index.
	IndexedEntries int `json:"indexed_entries"`
	// DataEntries counts active entries found by walking the data blocks.
	DataEntries int `json:"data_entries"`
	// MissingFromIndex lists entries present in the data blocks whose hash and
	// address are absent from the index.
	MissingFromIndex []string `json:"missing_from_index,omitempty"`
	// DanglingIndexEntries counts index entries that do not resolve to a
	// readable directory entry.
	DanglingIndexEntries int `json:"dangling_index_entries"`
	// HashMismatches lists entries whose name does not hash to the value the
	// index records for it.
	HashMismatches []string        `json:"hash_mismatches,omitempty"`
	Anomalies      []ReportAnomaly `json:"anomalies,omitempty"`
}

// Consistent reports whether the index and the data blocks agree.
func (r DirectoryIndexReport) Consistent() bool {
	return len(r.MissingFromIndex) == 0 &&
		len(r.HashMismatches) == 0 &&
		r.DanglingIndexEntries == 0
}

// VerifyDirectoryIndex cross-checks a directory's hash index against its data
// blocks. Directories with no index report HasIndex false and are consistent
// by definition.
func (v *Volume) VerifyDirectoryIndex(inodeNumber uint64) (DirectoryIndexReport, error) {
	report := DirectoryIndexReport{InodeNumber: inodeNumber}

	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return report, err
	}
	if !inode.IsDirectory() {
		return report, wrapParseError(0, "directory_inode", ErrInvalidInode)
	}

	listing, err := v.ScanDirectoryRecordsWithOptions(inodeNumber, DirectoryScanOptions{BestEffort: true})
	if err != nil {
		return report, err
	}
	report.Anomalies = append(report.Anomalies, listing.Anomalies...)

	indexEntries, err := v.collectDirectoryLeafEntries(inode)
	if err != nil {
		// No index is the normal case for short-form and single-block
		// directories; it is not a finding.
		return report, nil
	}
	report.HasIndex = true

	indexed := make(map[uint32]map[uint32]bool, len(indexEntries))
	for _, entry := range indexEntries {
		if entry.Address == 0 {
			continue
		}
		report.IndexedEntries++
		if indexed[entry.Hash] == nil {
			indexed[entry.Hash] = map[uint32]bool{}
		}
		indexed[entry.Hash][entry.Address] = true
	}

	seenAddresses := map[uint32]bool{}
	for _, record := range listing.Records {
		if record.Kind != RecordKindActive {
			continue
		}
		report.DataEntries++

		hash := xfsDirHashName([]byte(record.Name))
		address := uint32(record.LogicalOffset / dirDataAlign)
		seenAddresses[address] = true

		addresses, ok := indexed[hash]
		if !ok {
			report.MissingFromIndex = append(report.MissingFromIndex, record.Name)
			continue
		}
		if !addresses[address] {
			// The hash is present but points elsewhere: either the entry moved
			// without the index following, or the index was rewritten.
			report.HashMismatches = append(report.HashMismatches, record.Name)
		}
	}

	for _, entry := range indexEntries {
		if entry.Address == 0 {
			continue
		}
		if !seenAddresses[entry.Address] {
			report.DanglingIndexEntries++
		}
	}

	if !report.Consistent() {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{
			Code:     "directory_index_divergence",
			Severity: "warning",
			Inode:    inodeNumber,
			Message: fmt.Sprintf("hash index and data blocks disagree: %d missing, %d mismatched, %d dangling",
				len(report.MissingFromIndex), len(report.HashMismatches), report.DanglingIndexEntries),
		})
	}
	return report, nil
}

// VerifyDirectoryIndexByPath resolves a path and verifies its hash index.
func (v *Volume) VerifyDirectoryIndexByPath(path string) (DirectoryIndexReport, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return DirectoryIndexReport{}, err
	}
	return v.VerifyDirectoryIndex(inodeNumber)
}
