package libxfs

import (
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

const secondaryFeatureFlagFileType = 0x00000200

// ListDirectoryEntries lists entries for a directory inode.
//
// Currently this supports inline short-form directory data.
func (v *Volume) ListDirectoryEntries(inodeNumber uint64) ([]DirectoryEntry, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return nil, err
	}
	if !inode.IsDirectory() {
		return nil, wrapParseError(0, "directory_inode", ErrInvalidInode)
	}

	entries, err := parseShortFormDirectoryEntries(inode, v.ioh.formatVersion, v.ioh.secondaryFeatureFlags)
	if err == nil {
		return entries, nil
	}
	if !errors.Is(err, ErrUnsupportedDirFormat) {
		return nil, err
	}

	if inode.Size > math.MaxInt {
		return nil, wrapParseError(0, "inode_size", ErrInvalidInode)
	}
	data := make([]byte, int(inode.Size))
	n, readErr := v.ReadInodeData(inodeNumber, data, 0)
	if readErr != nil {
		if readErr == io.EOF {
			data = data[:n]
		} else {
			return nil, readErr
		}
	}
	if n < len(data) {
		data = data[:n]
	}
	if len(data) == 0 {
		return nil, readErr
	}
	records, parseErr := parseBlockDirectoryRecords(data, v.ioh.formatVersion, v.ioh.secondaryFeatureFlags, false)
	if parseErr != nil {
		return nil, parseErr
	}

	out := make([]DirectoryEntry, 0, len(records))
	for _, record := range records {
		if record.IsDeleted {
			continue
		}
		out = append(out, DirectoryEntry{Name: record.Name, InodeNumber: record.InodeNumber})
	}
	return out, nil
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

// ReadFileData reads all data bytes from a non-directory inode.
func (v *Volume) ReadFileData(inodeNumber uint64) ([]byte, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return nil, err
	}
	if inode.IsDirectory() {
		return nil, wrapParseError(0, "inode_type", ErrInvalidInode)
	}
	if inode.Size > math.MaxInt {
		return nil, wrapParseError(0, "inode_size", ErrInvalidInode)
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

// ResolveInodeByPath resolves an absolute path to an inode number.
//
// Currently this depends on ListDirectoryEntries, so path traversal is limited
// to inline short-form directories.
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
	current := v.sb.RootDirectoryInodeNumber

	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		entries, err := v.ListDirectoryEntries(current)
		if err != nil {
			return 0, err
		}

		next := uint64(0)
		for _, entry := range entries {
			if entry.Name == part {
				next = entry.InodeNumber
				break
			}
		}
		if next == 0 {
			return 0, fmt.Errorf("%w: %s", ErrInodeNotFound, part)
		}
		current = next
	}
	return current, nil
}

func parseShortFormDirectoryEntries(inode *Inode, formatVersion uint8, secondaryFeatureFlags uint32) ([]DirectoryEntry, error) {
	if inode == nil {
		return nil, wrapParseError(0, "inode", ErrInvalidInode)
	}
	if inode.ForkType != ForkTypeInlineData {
		return nil, fmt.Errorf("%w: only inline short-form directories are currently supported", ErrUnsupportedDirFormat)
	}

	data := inode.InlineData
	if len(data) < 6 {
		return nil, wrapParseError(0, "directory_header", ErrInvalidInode)
	}

	numberOf32BitEntries := int(data[0])
	numberOf64BitEntries := int(data[1])
	if numberOf32BitEntries != 0 && numberOf64BitEntries != 0 {
		return nil, wrapParseError(0, "directory_header", ErrInvalidInode)
	}

	inodeNumberSize := 4
	numberOfEntries := numberOf32BitEntries
	offset := 6
	if numberOf64BitEntries != 0 {
		inodeNumberSize = 8
		numberOfEntries = numberOf64BitEntries
		offset = 10
	}
	if offset > len(data) {
		return nil, wrapParseError(int64(offset), "directory_header", ErrInvalidInode)
	}

	hasFileType := formatVersion == 5 || (secondaryFeatureFlags&secondaryFeatureFlagFileType) != 0
	entries := make([]DirectoryEntry, 0, numberOfEntries)

	for i := 0; i < numberOfEntries; i++ {
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

		if hasFileType {
			if offset+1 > len(data) {
				return nil, wrapParseError(int64(offset), "directory_entry_type", ErrInvalidInode)
			}
			offset++
		}

		if offset+inodeNumberSize > len(data) {
			return nil, wrapParseError(int64(offset), "directory_entry_inode", ErrInvalidInode)
		}

		inodeNumber := uint64(0)
		if inodeNumberSize == 4 {
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
		offset += inodeNumberSize

		entries = append(entries, DirectoryEntry{Name: name, InodeNumber: inodeNumber})
	}

	return entries, nil
}
