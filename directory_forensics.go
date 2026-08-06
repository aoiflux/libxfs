package libxfs

import (
	"fmt"
)

const (
	directoryRecoveryConfidenceLow    = ConfidenceLow
	directoryRecoveryConfidenceMedium = ConfidenceMedium
	directoryRecoveryConfidenceHigh   = ConfidenceHigh
)

// directoryParseContext carries the per-block state needed to parse and label
// directory records.
type directoryParseContext struct {
	hasFileType    bool
	includeDeleted bool
	bestEffort     bool
	// includeDotEntries keeps the "." and ".." entries, which are otherwise
	// filtered out of listings. Used to recover a directory's parent link.
	includeDotEntries bool
	blockIndex        uint64
	blockOffset       uint64
}

// directoryHasFileType reports whether directory entries carry an ftype byte.
func directoryHasFileType(formatVersion uint8, secondaryFeatureFlags uint32) bool {
	return formatVersion == 5 || (secondaryFeatureFlags&secondaryFeatureFlagFileType) != 0
}

// parseBlockDirectoryRecords parses a single directory block.
//
// Deprecated in favour of parseDirectoryBlockRecords; retained because it is
// the narrow entry point used by existing callers and tests.
func parseBlockDirectoryRecords(data []byte, formatVersion uint8, secondaryFeatureFlags uint32, includeDeleted bool) ([]DirectoryRecord, error) {
	records, _, err := parseDirectoryBlockRecords(data, directoryParseContext{
		hasFileType:    directoryHasFileType(formatVersion, secondaryFeatureFlags),
		includeDeleted: includeDeleted,
	})
	return records, err
}

// parseDirectoryBlockRecords parses exactly one directory block.
//
// The block is framed by its own magic, so a data block ends at the block
// boundary while a block-format directory ends before its trailing leaf array
// and tail. Callers must pass one block at a time: passing a multi-block
// buffer would walk past the end of the first block's entry region.
func parseDirectoryBlockRecords(block []byte, ctx directoryParseContext) ([]DirectoryRecord, []ReportAnomaly, error) {
	// A directory block is at most XFS_MAX_BLOCKSIZE. Enforcing the format's
	// own bound keeps the record count derivable from the block size, so a
	// malformed block cannot drive unbounded accumulation.
	if len(block) > maxBlockSize {
		return nil, nil, fmt.Errorf("%w: directory block of %d bytes exceeds the %d byte maximum",
			ErrUnsupportedDirFormat, len(block), maxBlockSize)
	}

	kind, headerSize := classifyDirectoryBlock(block)

	switch kind {
	case dirBlockKindData, dirBlockKindBlock:
	case dirBlockKindEmpty:
		return nil, nil, nil
	default:
		return nil, nil, fmt.Errorf("%w: unsupported directory block kind %s", ErrUnsupportedDirFormat, kind)
	}

	if len(block) < headerSize {
		return nil, nil, wrapParseError(0, "directory_header", ErrInvalidInode)
	}

	entriesEnd := len(block)
	if kind == dirBlockKindBlock {
		end, err := blockDirectoryEntriesEnd(block, headerSize)
		if err != nil {
			return nil, nil, err
		}
		entriesEnd = end
	}

	records := make([]DirectoryRecord, 0, 8)
	var anomalies []ReportAnomaly

	// Resynchronisation budget. A block damaged badly enough to exhaust it is
	// noise, not data, and flooding the caller with anomalies helps nobody.
	anomalyBudget := maxAnomaliesPerDirectoryBlock

	// Offset at which the block's trailing zero padding begins. Computed once:
	// testing the remainder on every failure would make resynchronisation
	// quadratic in the block size on a badly damaged block.
	zeroTailStart := entriesEnd
	if ctx.bestEffort {
		for zeroTailStart > headerSize && block[zeroTailStart-1] == 0 {
			zeroTailStart--
		}
	}

	// recordFailure handles a framing failure. In strict mode the caller
	// returns the error; in best-effort mode it records an anomaly and reports
	// whether scanning should continue. Trailing zero padding ends the block
	// quietly: it is the normal tail of a partially used block, not damage.
	recordFailure := func(code string, offset int, message string) bool {
		if offset >= zeroTailStart {
			return false
		}
		if anomalyBudget > 0 {
			anomalies = append(anomalies, ctx.anomaly(code, offset, message))
			anomalyBudget--
			if anomalyBudget == 0 {
				anomalies = append(anomalies, ctx.anomaly("directory_anomaly_limit", offset,
					"further framing errors in this block suppressed"))
			}
		}
		return true
	}

	offset := headerSize
	for offset < entriesEnd {
		if entriesEnd-offset < dirUnusedHeaderSize {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(offset), "directory_entry", ErrInvalidInode)
			}
			recordFailure("directory_entry_truncated", offset, "trailing bytes too short to hold a directory entry")
			break
		}

		freeTag, ok := readUint16BE(block, offset)
		if !ok {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(offset), "directory_entry_tag", ErrInvalidInode)
			}
			recordFailure("directory_entry_tag", offset, "unreadable entry tag")
			break
		}

		if freeTag == dirDataFreeTag {
			recordLen, ok := readUint16BE(block, offset+2)
			if !ok || recordLen < dirUnusedHeaderSize || offset+int(recordLen) > entriesEnd {
				if !ctx.bestEffort {
					return nil, nil, wrapParseError(int64(offset), "directory_unused_len", ErrInvalidInode)
				}
				if !recordFailure("directory_unused_len", offset, fmt.Sprintf("invalid free-run length %d", recordLen)) {
					break
				}
				offset = nextAlignedOffset(offset)
				continue
			}

			if ctx.includeDeleted {
				records = append(records, DirectoryRecord{
					IsDeleted:         true,
					Kind:              RecordKindFreeSlot,
					Offset:            uint16(offset),
					RecordLength:      recordLen,
					Confidence:        ConfidenceLow,
					BlockIndex:        ctx.blockIndex,
					LogicalOffset:     ctx.blockOffset + uint64(offset),
					ConfidenceReasons: []string{ReasonInFreeSlot},
				})
				records = append(records, carveDeletedDirectoryEntries(block, offset, int(recordLen), entriesEnd, ctx)...)
			}
			offset += int(recordLen)
			continue
		}

		if entriesEnd-offset < dirEntryOverhead {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(offset), "directory_entry", ErrInvalidInode)
			}
			recordFailure("directory_entry_truncated", offset, "entry header runs past the end of the block")
			break
		}

		inodeNumber, ok := readUint64BE(block, offset)
		if !ok {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(offset), "directory_entry_inode", ErrInvalidInode)
			}
			if !recordFailure("directory_entry_inode", offset, "unreadable inode number") {
				break
			}
			offset = nextAlignedOffset(offset)
			continue
		}

		nameSize := int(block[offset+8])

		// A real entry always names something and always points at an inode.
		// Inode 0 does not exist in XFS, and a zero length name cannot be
		// stored, so either means the framing has been lost.
		if nameSize == 0 || inodeNumber == 0 {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(offset), "directory_entry_identity", ErrInvalidInode)
			}
			if !recordFailure("directory_entry_identity", offset,
				fmt.Sprintf("entry has name length %d and inode %d", nameSize, inodeNumber)) {
				break
			}
			offset = nextAlignedOffset(offset)
			continue
		}

		recordLen := directoryEntryLength(nameSize, ctx.hasFileType)

		if recordLen < dirEntryOverhead || offset+recordLen > entriesEnd {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(offset), "directory_entry_len", ErrInvalidInode)
			}
			if !recordFailure("directory_entry_len", offset,
				fmt.Sprintf("entry length %d runs past the entry region", recordLen)) {
				break
			}
			offset = nextAlignedOffset(offset)
			continue
		}

		nameStart := offset + 9
		nameEnd := nameStart + nameSize
		if nameEnd > entriesEnd {
			if !ctx.bestEffort {
				return nil, nil, wrapParseError(int64(nameStart), "directory_entry_name", ErrInvalidInode)
			}
			if !recordFailure("directory_entry_name", offset, "name runs past the entry region") {
				break
			}
			offset = nextAlignedOffset(offset)
			continue
		}
		name := string(block[nameStart:nameEnd])

		fileType := DirEntryFileTypeUnknown
		if ctx.hasFileType {
			fileType = block[nameEnd]
		}

		if ctx.includeDotEntries || (name != "." && name != "..") {
			reasons := []string{ReasonIntactFraming}
			if ctx.hasFileType && fileType < dirEntryFileTypeMax {
				reasons = append(reasons, ReasonFileTypeValid)
			}
			records = append(records, DirectoryRecord{
				Name:              name,
				InodeNumber:       inodeNumber,
				Kind:              RecordKindActive,
				FileType:          fileType,
				Offset:            uint16(offset),
				RecordLength:      uint16(recordLen),
				Confidence:        ConfidenceHigh,
				BlockIndex:        ctx.blockIndex,
				LogicalOffset:     ctx.blockOffset + uint64(offset),
				ConfidenceReasons: reasons,
			})
		}
		offset += recordLen
	}

	return records, anomalies, nil
}

// blockDirectoryEntriesEnd returns the offset at which the entry region of a
// block-format directory ends, i.e. where the leaf array and tail begin.
func blockDirectoryEntriesEnd(block []byte, headerSize int) (int, error) {
	if len(block) < headerSize+dirBlockTailSize {
		return 0, wrapParseError(0, "directory_tail", ErrInvalidInode)
	}

	tailOffset := len(block) - dirBlockTailSize
	entryCount, ok := readUint32BE(block, tailOffset)
	if !ok {
		return 0, wrapParseError(int64(tailOffset), "directory_footer", ErrInvalidInode)
	}

	tailSize := uint64(dirBlockTailSize) + uint64(entryCount)*dirLeafEntrySize
	if tailSize > uint64(len(block)-headerSize) {
		return 0, wrapParseError(int64(tailOffset), "directory_tail", ErrInvalidInode)
	}
	return len(block) - int(tailSize), nil
}

// directoryEntryLength returns the on-disk size of an active directory entry:
// inumber(8) + namelen(1) + name + optional ftype(1) + tag(2), rounded up to
// an 8 byte boundary.
func directoryEntryLength(nameSize int, hasFileType bool) int {
	length := 9 + nameSize + 2
	if hasFileType {
		length++
	}
	if rem := length % dirDataAlign; rem != 0 {
		length += dirDataAlign - rem
	}
	return length
}

// nextAlignedOffset advances to the next 8 byte boundary strictly greater than
// offset, so best-effort resynchronisation always makes progress.
func nextAlignedOffset(offset int) int {
	return offset + dirDataAlign - (offset % dirDataAlign)
}

func (ctx directoryParseContext) anomaly(code string, offset int, message string) ReportAnomaly {
	return ReportAnomaly{
		Code:     code,
		Severity: "warning",
		Message: fmt.Sprintf("directory block %d offset %d: %s",
			ctx.blockIndex, ctx.blockOffset+uint64(offset), message),
	}
}

// carveDeletedDirectoryEntries scans a free run for structures that still look
// like directory entries. Everything it returns is a probabilistic candidate:
// the space is reclaimed, so a match may be a stale entry, a partial overwrite,
// or an entirely coincidental byte pattern.
func carveDeletedDirectoryEntries(data []byte, slotOffset int, slotLength int, entriesEnd int, ctx directoryParseContext) []DirectoryRecord {
	start := slotOffset + dirUnusedHeaderSize
	end := slotOffset + slotLength
	if end > entriesEnd {
		end = entriesEnd
	}
	if end-start < dirEntryOverhead {
		return nil
	}

	carved := make([]DirectoryRecord, 0, 2)
	cursor := start
	for cursor+dirEntryOverhead <= end {
		inodeNumber, ok := readUint64BE(data, cursor)
		if !ok || inodeNumber == 0 {
			cursor++
			continue
		}

		nameSize := int(data[cursor+8])
		if nameSize <= 0 || nameSize > maxDirectoryNameBytes {
			cursor++
			continue
		}

		recordLen := directoryEntryLength(nameSize, ctx.hasFileType)
		if recordLen < dirEntryOverhead || cursor+recordLen > end {
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

		reasons := []string{ReasonInFreeSlot, ReasonNamePrintable}
		confidence := ConfidenceMedium

		if cursor%dirDataAlign == 0 {
			reasons = append(reasons, ReasonAlignedOffset)
		}

		fileType := DirEntryFileTypeUnknown
		if ctx.hasFileType {
			fileType = data[nameEnd]
			if fileType < dirEntryFileTypeMax {
				reasons = append(reasons, ReasonFileTypeValid)
			}
		}

		// The trailing tag of an intact entry records its own start offset.
		// A match is strong evidence that this is a real, unmodified record
		// rather than a coincidental byte pattern.
		tagOffset := cursor + recordLen - 2
		tagValue, ok := readUint16BE(data, tagOffset)
		if ok && int(tagValue) == cursor {
			confidence = ConfidenceHigh
			reasons = append(reasons, ReasonTagMatchesOffset)
		}

		carved = append(carved, DirectoryRecord{
			Name:              name,
			InodeNumber:       inodeNumber,
			IsDeleted:         true,
			IsCarved:          true,
			Kind:              RecordKindCarved,
			FileType:          fileType,
			Offset:            uint16(cursor),
			RecordLength:      uint16(recordLen),
			Confidence:        confidence,
			BlockIndex:        ctx.blockIndex,
			LogicalOffset:     ctx.blockOffset + uint64(cursor),
			ConfidenceReasons: reasons,
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
//
// Records carrying IsCarved are probabilistic: see DirectoryRecord.Kind and
// IsProbabilistic before presenting them as fact.
func (v *Volume) ScanDirectoryRecords(inodeNumber uint64) ([]DirectoryRecord, error) {
	listing, err := v.ScanDirectoryRecordsWithOptions(inodeNumber, DirectoryScanOptions{})
	if err != nil {
		return nil, err
	}
	return listing.Records, nil
}

// ScanDirectoryRecordsByPath resolves a directory path and scans its records.
func (v *Volume) ScanDirectoryRecordsByPath(path string) ([]DirectoryRecord, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return nil, err
	}
	return v.ScanDirectoryRecords(inodeNumber)
}
