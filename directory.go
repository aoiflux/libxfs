package libxfs

import (
	"fmt"
	"io"
	"math"
	"strings"
)

// secondaryFeatureFlagFileType is XFS_SB_VERSION2_FTYPEBIT: directory entries
// carry an ftype byte. On v5 filesystems ftype is always present.
const secondaryFeatureFlagFileType = 0x00000200

// maxPathTraversalDepth bounds path resolution so that a crafted image cannot
// drive unbounded traversal.
const maxPathTraversalDepth = 1024

// ListDirectoryEntries lists active entries for a directory inode.
//
// Short-form, block, leaf, node and btree directories are all supported:
// entries always live in the directory's data-block region, which is walked
// one directory block at a time.
// Entries recovered before a failure are returned alongside the error: on a
// damaged image the blocks that did parse are evidence, and discarding them
// because a later block did not is how a recursive walk loses whole subtrees.
// Use ListDirectoryEntriesReport when the completeness of the listing matters.
func (v *Volume) ListDirectoryEntries(inodeNumber uint64) ([]DirectoryEntry, error) {
	listing, err := v.scanDirectory(inodeNumber, DirectoryScanOptions{})
	return listing.Entries, err
}

// ListDirectoryEntriesReport lists a directory and reports how the scan went.
//
// ListDirectoryEntries returns only the entries, so a caller cannot tell a
// complete listing from one that stopped at a cap or skipped an unreadable
// block. The returned DirectoryListing carries Truncated, Anomalies,
// BlocksScanned and SourceFormat for callers that must know.
func (v *Volume) ListDirectoryEntriesReport(inodeNumber uint64) (DirectoryListing, error) {
	return v.scanDirectory(inodeNumber, DirectoryScanOptions{})
}

// ListDirectoryEntriesReportByPath resolves a path and lists it with a report.
func (v *Volume) ListDirectoryEntriesReportByPath(path string) (DirectoryListing, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return DirectoryListing{}, err
	}
	return v.ListDirectoryEntriesReport(inodeNumber)
}

// ListRootDirectoryEntries lists entries for the root directory inode.
func (v *Volume) ListRootDirectoryEntries() ([]DirectoryEntry, error) {
	if v.sb == nil {
		return nil, wrapParseError(0, "superblock", ErrInvalidSuperblock)
	}
	return v.ListDirectoryEntries(v.sb.RootDirectoryInodeNumber)
}

// ListDirectoryEntriesByPath resolves a directory path and lists its entries.
func (v *Volume) ListDirectoryEntriesByPath(path string) ([]DirectoryEntry, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return nil, err
	}
	return v.ListDirectoryEntries(inodeNumber)
}

// OpenInodeByPath resolves an absolute path and opens the corresponding inode.
func (v *Volume) OpenInodeByPath(path string) (*Inode, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return nil, err
	}
	return v.OpenInode(inodeNumber)
}

// DirectoryParentInode returns the inode number a directory's ".." refers to.
//
// For short-form directories the parent is stored in the directory header; for
// block-backed directories it is the ".." entry in the first data block. It is
// the only in-inode link back up the tree, which makes it the starting point
// for reconstructing the path of an orphaned directory.
func (v *Volume) DirectoryParentInode(inodeNumber uint64) (uint64, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return 0, err
	}
	if !inode.IsDirectory() {
		return 0, wrapParseError(0, "directory_inode", ErrInvalidInode)
	}

	if inode.ForkType == ForkTypeInlineData {
		header, err := parseShortFormDirectoryHeader(inode.InlineData)
		if err != nil {
			return 0, err
		}
		return header.parentInodeNumber, nil
	}

	block, err := v.readDirectoryBlock(inode, 0)
	if err != nil {
		return 0, err
	}

	records, _, err := parseDirectoryBlockRecords(block, directoryParseContext{
		hasFileType:       directoryHasFileType(v.ioh.formatVersion, v.ioh.secondaryFeatureFlags),
		includeDotEntries: true,
	})
	if err != nil {
		return 0, err
	}
	for _, record := range records {
		if record.Name == ".." {
			return record.InodeNumber, nil
		}
	}
	return 0, fmt.Errorf("%w: parent entry not found", ErrInodeNotFound)
}

// DirectoryParentInodeByPath resolves a directory path and returns its parent
// inode number.
func (v *Volume) DirectoryParentInodeByPath(path string) (uint64, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return 0, err
	}
	return v.DirectoryParentInode(inodeNumber)
}

// ReadFileData reads all data bytes from a non-directory inode.
func (v *Volume) ReadFileData(inodeNumber uint64) ([]byte, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return nil, err
	}
	if inode.IsDirectory() {
		return nil, wrapParseError(0, "inode_type", ErrInvalidInode)
	}
	if err := v.checkPlausibleSize(inode.Size); err != nil {
		return nil, err
	}

	out := make([]byte, int(inode.Size))
	n, err := v.ReadInodeData(inodeNumber, out, 0)
	if err != nil && err != io.EOF {
		return nil, err
	}
	if n < len(out) {
		out = out[:n]
	}
	return out, nil
}

// ReadFileDataByPath resolves an absolute path and reads all file data bytes.
func (v *Volume) ReadFileDataByPath(path string) ([]byte, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return nil, err
	}
	return v.ReadFileData(inodeNumber)
}

// checkPlausibleSize rejects sizes that exceed the capacity of the volume.
//
// A recorded size is attacker controlled on a crafted image; allocating it
// blindly is an out-of-memory vector. No object can be larger than the
// filesystem that holds it.
func (v *Volume) checkPlausibleSize(size uint64) error {
	if size > math.MaxInt {
		return wrapParseError(0, "inode_size", ErrInvalidInode)
	}
	capacity := v.volumeCapacityBytes()
	if capacity != 0 && size > capacity {
		return wrapParseError(int64(size), "inode_size", ErrInvalidInode)
	}
	return nil
}

// volumeCapacityBytes returns the addressable size of the volume, or zero when
// the geometry is unknown. Nothing stored on the filesystem can be larger.
func (v *Volume) volumeCapacityBytes() uint64 {
	if v.sb == nil || v.sb.NumberOfBlocks == 0 || v.sb.BlockSize == 0 {
		return 0
	}
	blockSize := uint64(v.sb.BlockSize)
	if v.sb.NumberOfBlocks > math.MaxUint64/blockSize {
		return 0
	}
	return v.sb.NumberOfBlocks * blockSize
}

// ResolveInodeByPath resolves an absolute path to an inode number.
func (v *Volume) ResolveInodeByPath(path string) (uint64, error) {
	if path == "" {
		return 0, wrapParseError(0, "path", ErrInvalidPath)
	}
	if v.sb == nil {
		return 0, wrapParseError(0, "superblock", ErrInvalidSuperblock)
	}

	if path == "/" {
		return v.sb.RootDirectoryInodeNumber, nil
	}
	if !strings.HasPrefix(path, "/") {
		return 0, wrapParseError(0, "path", ErrInvalidPath)
	}

	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > maxPathTraversalDepth {
		return 0, fmt.Errorf("%w: path exceeds maximum traversal depth", ErrInvalidPath)
	}

	current := v.sb.RootDirectoryInodeNumber
	visited := map[uint64]struct{}{current: {}}

	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		next, err := v.lookupDirectoryEntry(current, part)
		if err != nil {
			return 0, err
		}
		if next == 0 {
			return 0, fmt.Errorf("%w: %s", ErrInodeNotFound, part)
		}
		if _, loop := visited[next]; loop {
			return 0, fmt.Errorf("%w: directory loop at %s", ErrInvalidPath, part)
		}
		visited[next] = struct{}{}
		current = next
	}
	return current, nil
}

// lookupDirectoryEntry resolves one name within a directory, returning zero
// when the name is not present.
//
// It tries the hash index first, which avoids reading every data block, and
// falls back to a full linear walk whenever the index is absent, unreadable, or
// does not produce a match. The fallback is mandatory: an index is an
// optimisation, and an optimisation must never reduce what can be recovered
// from a damaged image.
func (v *Volume) lookupDirectoryEntry(directoryInode uint64, name string) (uint64, error) {
	if inode, err := v.OpenInode(directoryInode); err == nil && inode.IsDirectory() {
		if entry, ok := v.lookupDirectoryEntryByHash(inode, name); ok {
			return entry.InodeNumber, nil
		}
	}

	// The entries recovered before any failure are searched first. A directory
	// with one unreadable block still resolves every name held in the blocks
	// that were readable, and failing the whole lookup would strand the entire
	// subtree below it.
	entries, err := v.ListDirectoryEntries(directoryInode)
	for _, entry := range entries {
		if entry.Name == name {
			return entry.InodeNumber, nil
		}
	}
	if err != nil {
		return 0, err
	}
	return 0, nil
}

// shortFormDirectoryHeader is the fixed header of an inline directory.
type shortFormDirectoryHeader struct {
	numberOfEntries   int
	parentInodeNumber uint64
	inodeNumberSize   int
	headerSize        int
}

// parseShortFormDirectoryHeader decodes the xfs_dir2_sf_hdr at the start of an
// inline directory: count(1), i8count(1), parent(4 or 8).
func parseShortFormDirectoryHeader(data []byte) (shortFormDirectoryHeader, error) {
	header := shortFormDirectoryHeader{}
	if len(data) < shortFormHeaderSize32 {
		return header, wrapParseError(0, "directory_header", ErrInvalidInode)
	}

	numberOf32BitEntries := int(data[0])
	numberOf64BitEntries := int(data[1])
	if numberOf32BitEntries != 0 && numberOf64BitEntries != 0 {
		return header, wrapParseError(0, "directory_header", ErrInvalidInode)
	}

	header.inodeNumberSize = shortFormInodeSize32
	header.numberOfEntries = numberOf32BitEntries
	header.headerSize = shortFormHeaderSize32
	if numberOf64BitEntries != 0 {
		header.inodeNumberSize = shortFormInodeSize64
		header.numberOfEntries = numberOf64BitEntries
		header.headerSize = shortFormHeaderSize64
	}
	if header.headerSize > len(data) {
		return header, wrapParseError(int64(header.headerSize), "directory_header", ErrInvalidInode)
	}

	if header.inodeNumberSize == 4 {
		parent, ok := readUint32BE(data, shortFormOffsetParent)
		if !ok {
			return header, wrapParseError(shortFormOffsetParent, "directory_parent_inode", ErrInvalidInode)
		}
		header.parentInodeNumber = uint64(parent)
	} else {
		parent, ok := readUint64BE(data, shortFormOffsetParent)
		if !ok {
			return header, wrapParseError(shortFormOffsetParent, "directory_parent_inode", ErrInvalidInode)
		}
		header.parentInodeNumber = parent
	}

	return header, nil
}

func parseShortFormDirectoryEntries(inode *Inode, formatVersion uint8, secondaryFeatureFlags uint32) ([]DirectoryEntry, error) {
	if inode == nil {
		return nil, wrapParseError(0, "inode", ErrInvalidInode)
	}
	if inode.ForkType != ForkTypeInlineData {
		return nil, fmt.Errorf("%w: directory is not in short-form inline format", ErrUnsupportedDirFormat)
	}

	data := inode.InlineData
	header, err := parseShortFormDirectoryHeader(data)
	if err != nil {
		return nil, err
	}

	hasFileType := directoryHasFileType(formatVersion, secondaryFeatureFlags)
	entries := make([]DirectoryEntry, 0, header.numberOfEntries)
	offset := header.headerSize

	for i := 0; i < header.numberOfEntries; i++ {
		if offset+1 > len(data) {
			return nil, wrapParseError(int64(offset), "directory_entry_header", ErrInvalidInode)
		}
		nameSize := int(data[offset])
		offset++

		if offset+2 > len(data) {
			return nil, wrapParseError(int64(offset), "directory_entry_tag", ErrInvalidInode)
		}
		offset += 2

		if offset+nameSize > len(data) {
			return nil, wrapParseError(int64(offset), "directory_entry_name", ErrInvalidInode)
		}
		name := string(data[offset : offset+nameSize])
		offset += nameSize

		fileType := DirEntryFileTypeUnknown
		if hasFileType {
			if offset+1 > len(data) {
				return nil, wrapParseError(int64(offset), "directory_entry_type", ErrInvalidInode)
			}
			fileType = data[offset]
			offset++
		}

		if offset+header.inodeNumberSize > len(data) {
			return nil, wrapParseError(int64(offset), "directory_entry_inode", ErrInvalidInode)
		}

		inodeNumber := uint64(0)
		if header.inodeNumberSize == 4 {
			value, ok := readUint32BE(data, offset)
			if !ok {
				return nil, wrapParseError(int64(offset), "directory_entry_inode", ErrInvalidInode)
			}
			inodeNumber = uint64(value)
		} else {
			value, ok := readUint64BE(data, offset)
			if !ok {
				return nil, wrapParseError(int64(offset), "directory_entry_inode", ErrInvalidInode)
			}
			inodeNumber = value
		}
		offset += header.inodeNumberSize

		entries = append(entries, DirectoryEntry{
			Name:        name,
			InodeNumber: inodeNumber,
			FileType:    fileType,
		})
	}

	return entries, nil
}
