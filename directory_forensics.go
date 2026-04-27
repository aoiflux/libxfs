package libxfs

import (
	"fmt"
	"io"
	"math"
)

const (
	directoryRecoveryConfidenceLow    = "low"
	directoryRecoveryConfidenceMedium = "medium"
	directoryRecoveryConfidenceHigh   = "high"
)

func parseBlockDirectoryRecords(data []byte, formatVersion uint8, secondaryFeatureFlags uint32, includeDeleted bool) ([]DirectoryRecord, error) {
	if len(data) < 16 {
		return nil, wrapParseError(0, "directory_block", ErrInvalidInode)
	}

	sig := string(data[0:4])
	hasFooter := false
	headerSize := 0
	switch sig {
	case "XD2B":
		hasFooter = true
		headerSize = 16
	case "XD2D":
		headerSize = 16
	case "XDB3":
		hasFooter = true
		headerSize = 56
	case "XDD3":
		headerSize = 56
	default:
		return nil, fmt.Errorf("%w: unsupported block directory signature %q", ErrUnsupportedDirFormat, sig)
	}
	if len(data) < headerSize {
		return nil, wrapParseError(0, "directory_header", ErrInvalidInode)
	}

	entriesEnd := len(data)
	if hasFooter {
		if len(data) < 8 {
			return nil, wrapParseError(0, "directory_footer", ErrInvalidInode)
		}
		entryCount, ok := readUint32BE(data, len(data)-8)
		if !ok {
			return nil, wrapParseError(int64(len(data)-8), "directory_footer", ErrInvalidInode)
		}

		hashBytes := int(entryCount) * 8
		tailSize := 8 + hashBytes
		if tailSize < 8 || tailSize > len(data)-headerSize {
			return nil, wrapParseError(int64(len(data)-8), "directory_tail", ErrInvalidInode)
		}
		entriesEnd = len(data) - tailSize
	}

	hasFileType := formatVersion == 5 || (secondaryFeatureFlags&secondaryFeatureFlagFileType) != 0
	records := make([]DirectoryRecord, 0, 8)

	offset := headerSize
	for offset < entriesEnd {
		if entriesEnd-offset < 4 {
			return nil, wrapParseError(int64(offset), "directory_entry", ErrInvalidInode)
		}

		freeTag, ok := readUint16BE(data, offset)
		if !ok {
			return nil, wrapParseError(int64(offset), "directory_entry_tag", ErrInvalidInode)
		}
		if freeTag == 0xffff {
			recordLen, ok := readUint16BE(data, offset+2)
			if !ok {
				return nil, wrapParseError(int64(offset+2), "directory_unused_len", ErrInvalidInode)
			}
			if recordLen < 4 || offset+int(recordLen) > entriesEnd {
				return nil, wrapParseError(int64(offset), "directory_unused_len", ErrInvalidInode)
			}
			if includeDeleted {
				records = append(records, DirectoryRecord{
					IsDeleted:    true,
					Offset:       uint16(offset),
					RecordLength: recordLen,
					Confidence:   directoryRecoveryConfidenceLow,
				})
				carved := carveDeletedDirectoryEntries(data, offset, int(recordLen), entriesEnd, hasFileType)
				records = append(records, carved...)
			}
			offset += int(recordLen)
			continue
		}

		if entriesEnd-offset < 11 {
			return nil, wrapParseError(int64(offset), "directory_entry", ErrInvalidInode)
		}
		inodeNumber, ok := readUint64BE(data, offset)
		if !ok {
			return nil, wrapParseError(int64(offset), "directory_entry_inode", ErrInvalidInode)
		}
		nameSize := int(data[offset+8])

		recordLen := 9 + nameSize + 2
		if hasFileType {
			recordLen++
		}
		if rem := recordLen % 8; rem != 0 {
			recordLen += 8 - rem
		}
		if recordLen < 11 || offset+recordLen > entriesEnd {
			return nil, wrapParseError(int64(offset), "directory_entry_len", ErrInvalidInode)
		}

		nameStart := offset + 9
		nameEnd := nameStart + nameSize
		if nameEnd > entriesEnd {
			return nil, wrapParseError(int64(nameStart), "directory_entry_name", ErrInvalidInode)
		}
		name := string(data[nameStart:nameEnd])

		if name != "." && name != ".." {
			records = append(records, DirectoryRecord{
				Name:         name,
				InodeNumber:  inodeNumber,
				Offset:       uint16(offset),
				RecordLength: uint16(recordLen),
				Confidence:   directoryRecoveryConfidenceHigh,
			})
		}
		offset += recordLen
	}

	return records, nil
}

func carveDeletedDirectoryEntries(data []byte, slotOffset int, slotLength int, entriesEnd int, hasFileType bool) []DirectoryRecord {
	start := slotOffset + 4
	end := slotOffset + slotLength
	if end > entriesEnd {
		end = entriesEnd
	}
	if end-start < 11 {
		return nil
	}

	carved := make([]DirectoryRecord, 0, 2)
	cursor := start
	for cursor+11 <= end {
		inodeNumber, ok := readUint64BE(data, cursor)
		if !ok || inodeNumber == 0 {
			cursor++
			continue
		}

		nameSize := int(data[cursor+8])
		if nameSize <= 0 || nameSize > 255 {
			cursor++
			continue
		}

		recordLen := 9 + nameSize + 2
		if hasFileType {
			recordLen++
		}
		if rem := recordLen % 8; rem != 0 {
			recordLen += 8 - rem
		}
		if recordLen < 11 || cursor+recordLen > end {
			cursor++
			continue
		}

		nameStart := cursor + 9
		nameEnd := nameStart + nameSize
		if nameEnd > end {
			cursor++
			continue
		}
		nameBytes := data[nameStart:nameEnd]
		if !looksLikeDirectoryName(nameBytes) {
			cursor++
			continue
		}

		name := string(nameBytes)
		if name == "." || name == ".." {
			cursor++
			continue
		}

		confidence := directoryRecoveryConfidenceMedium
		tagOffset := cursor + recordLen - 2
		tagValue, ok := readUint16BE(data, tagOffset)
		if ok && int(tagValue) == cursor {
			confidence = directoryRecoveryConfidenceHigh
		}

		carved = append(carved, DirectoryRecord{
			Name:         name,
			InodeNumber:  inodeNumber,
			IsDeleted:    true,
			IsCarved:     true,
			Offset:       uint16(cursor),
			RecordLength: uint16(recordLen),
			Confidence:   confidence,
		})

		cursor += recordLen
	}

	return carved
}

func looksLikeDirectoryName(name []byte) bool {
	if len(name) == 0 {
		return false
	}
	for _, b := range name {
		if b < 0x20 || b > 0x7e {
			return false
		}
		if b == '/' || b == '\\' || b == 0 {
			return false
		}
	}
	return true
}

// ScanDirectoryRecords lists active and deleted directory records for an inode.
func (v *Volume) ScanDirectoryRecords(inodeNumber uint64) ([]DirectoryRecord, error) {
	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return nil, err
	}
	if !inode.IsDirectory() {
		return nil, wrapParseError(0, "directory_inode", ErrInvalidInode)
	}

	entries, err := parseShortFormDirectoryEntries(inode, v.ioh.formatVersion, v.ioh.secondaryFeatureFlags)
	if err == nil {
		records := make([]DirectoryRecord, 0, len(entries))
		for _, entry := range entries {
			records = append(records, DirectoryRecord{Name: entry.Name, InodeNumber: entry.InodeNumber})
		}
		return records, nil
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
		return nil, wrapParseError(0, "directory_data", ErrInvalidInode)
	}
	return parseBlockDirectoryRecords(data, v.ioh.formatVersion, v.ioh.secondaryFeatureFlags, true)
}

// ScanDirectoryRecordsByPath resolves a directory path and scans its records.
func (v *Volume) ScanDirectoryRecordsByPath(path string) ([]DirectoryRecord, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return nil, err
	}
	return v.ScanDirectoryRecords(inodeNumber)
}
