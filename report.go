package libxfs

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"
	"strings"
	"time"
)

// VerificationMode defines how report generation handles verification failures.
type VerificationMode string

const (
	// VerificationModeBestEffort records anomalies and continues report generation.
	VerificationModeBestEffort VerificationMode = "best_effort"
	// VerificationModeStrict fails report generation on verification mismatch.
	VerificationModeStrict VerificationMode = "strict"
)

// ReportProvenance captures parser and verification context for reproducibility.
type ReportProvenance struct {
	ParserVersion        string           `json:"parser_version"`
	VerificationMode     VerificationMode `json:"verification_mode"`
	Coverage             []string         `json:"coverage"`
	SuperblockCRCChecked bool             `json:"superblock_crc_checked"`
	InodeCRCChecked      bool             `json:"inode_crc_checked"`
}

// ReportOptions controls how volume-level report generation behaves.
type ReportOptions struct {
	// RootPath selects the start path for traversal. Defaults to "/".
	RootPath string
	// MaxEntries limits the number of discovered inodes in Files.
	// Zero or negative means unlimited.
	MaxEntries int
	// IncludeDirectoryArtifacts includes deleted/carved directory record output
	// for each visited directory inode.
	IncludeDirectoryArtifacts bool
	// VerificationMode controls whether checksum/verification mismatches are
	// fatal (`strict`) or recorded as anomalies (`best_effort`).
	VerificationMode VerificationMode
}

// ReportAnomaly captures a parsing or consistency concern encountered while
// building reports.
type ReportAnomaly struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Path     string `json:"path,omitempty"`
	Inode    uint64 `json:"inode,omitempty"`
}

// VolumeIntegrityReport summarizes core XFS geometry and validation findings.
type VolumeIntegrityReport struct {
	Type                     string          `json:"type"`
	FormatVersion            uint8           `json:"format_version"`
	BlockSize                uint32          `json:"block_size"`
	InodeSize                uint16          `json:"inode_size"`
	DirectoryBlockSize       uint32          `json:"directory_block_size"`
	RootDirectoryInodeNumber uint64          `json:"root_directory_inode"`
	AllocationGroupSize      uint32          `json:"allocation_group_size"`
	NumberOfAllocationGroups uint32          `json:"number_of_allocation_groups"`
	NumberOfBlocks           uint64          `json:"number_of_blocks"`
	VolumeLabel              string          `json:"volume_label,omitempty"`
	SuperblockCRCChecked     bool            `json:"superblock_crc_checked"`
	SuperblockCRCValid       bool            `json:"superblock_crc_valid"`
	Anomalies                []ReportAnomaly `json:"anomalies,omitempty"`
}

// InodeFragment describes one extent from the inode data fork.
type InodeFragment struct {
	StartOffset         uint64 `json:"start_offset"`
	EndOffset           uint64 `json:"end_offset"`
	LengthBytes         uint64 `json:"length_bytes"`
	LogicalBlockNumber  uint64 `json:"logical_block_number"`
	PhysicalBlockNumber uint64 `json:"physical_block_number"`
	NumberOfBlocks      uint32 `json:"number_of_blocks"`
	IsSparse            bool   `json:"is_sparse"`
}

// InodeForensicReport is structured metadata for one inode.
type InodeForensicReport struct {
	InodeNumber            uint64              `json:"inode_number"`
	Path                   string              `json:"path,omitempty"`
	Type                   string              `json:"type"`
	FileMode               uint16              `json:"file_mode"`
	ForkType               uint8               `json:"fork_type"`
	Size                   uint64              `json:"size"`
	OwnerID                uint32              `json:"owner_id"`
	GroupID                uint32              `json:"group_id"`
	NumberOfLinks          uint32              `json:"number_of_links"`
	AccessTime             time.Time           `json:"access_time"`
	ModificationTime       time.Time           `json:"modification_time"`
	InodeChangeTime        time.Time           `json:"inode_change_time"`
	CreationTime           time.Time           `json:"creation_time,omitempty"`
	DataExtentCount        int                 `json:"data_extent_count"`
	AttributesExtentCount  int                 `json:"attributes_extent_count"`
	HasInlineData          bool                `json:"has_inline_data"`
	Fragmentation          FragmentationReport `json:"fragmentation"`
	Fragments              []InodeFragment     `json:"fragments,omitempty"`
	ExtendedAttributeNames []string            `json:"extended_attribute_names,omitempty"`
	Anomalies              []ReportAnomaly     `json:"anomalies,omitempty"`
}

// DirectoryArtifactReport summarizes active/deleted/carved records for one
// directory inode.
type DirectoryArtifactReport struct {
	InodeNumber  uint64            `json:"inode_number"`
	Path         string            `json:"path,omitempty"`
	RecordCount  int               `json:"record_count"`
	ActiveCount  int               `json:"active_count"`
	DeletedCount int               `json:"deleted_count"`
	CarvedCount  int               `json:"carved_count"`
	Records      []DirectoryRecord `json:"records"`
	Anomalies    []ReportAnomaly   `json:"anomalies,omitempty"`
}

// XFSReport is a combined volume + inode + directory-artifact report.
type XFSReport struct {
	GeneratedAt        time.Time                 `json:"generated_at"`
	RootPath           string                    `json:"root_path"`
	Provenance         ReportProvenance          `json:"provenance"`
	Volume             VolumeIntegrityReport     `json:"volume"`
	Files              []InodeForensicReport     `json:"files"`
	DirectoryArtifacts []DirectoryArtifactReport `json:"directory_artifacts,omitempty"`
	Anomalies          []ReportAnomaly           `json:"anomalies,omitempty"`
}

// Summary returns a human-readable summary of the report.
func (r *XFSReport) Summary() string {
	if r == nil {
		return ""
	}

	dirs := 0
	files := 0
	for _, file := range r.Files {
		if file.Type == "directory" {
			dirs++
		} else {
			files++
		}
	}

	deletedRecords := 0
	carvedRecords := 0
	for _, artifact := range r.DirectoryArtifacts {
		deletedRecords += artifact.DeletedCount
		carvedRecords += artifact.CarvedCount
	}

	var b strings.Builder
	fmt.Fprintf(&b, "=== XFS Forensic Report Summary ===\n")
	fmt.Fprintf(&b, "Generated: %s\n", r.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(&b, "Parser: %s mode=%s\n", r.Provenance.ParserVersion, r.Provenance.VerificationMode)
	fmt.Fprintf(&b, "Root path: %s\n", r.RootPath)
	fmt.Fprintf(&b, "Filesystem: %s v%d block=%d inode=%d dir_block=%d\n",
		r.Volume.Type,
		r.Volume.FormatVersion,
		r.Volume.BlockSize,
		r.Volume.InodeSize,
		r.Volume.DirectoryBlockSize,
	)
	fmt.Fprintf(&b, "Inodes reported: %d (directories=%d files=%d)\n", len(r.Files), dirs, files)
	fmt.Fprintf(&b, "Directory artifact reports: %d (deleted=%d carved=%d)\n", len(r.DirectoryArtifacts), deletedRecords, carvedRecords)
	fmt.Fprintf(&b, "Volume anomalies: %d\n", len(r.Volume.Anomalies))
	fmt.Fprintf(&b, "Report anomalies: %d\n", len(r.Anomalies))
	return b.String()
}

// VolumeIntegrityReport builds a metadata/geometry report for the open volume.
func (v *Volume) VolumeIntegrityReport() (VolumeIntegrityReport, error) {
	return v.volumeIntegrityReportWithMode(VerificationModeBestEffort)
}

func (v *Volume) volumeIntegrityReportWithMode(mode VerificationMode) (VolumeIntegrityReport, error) {
	if v.IsClosed() {
		return VolumeIntegrityReport{}, ErrVolumeClosed
	}
	if v.sb == nil {
		return VolumeIntegrityReport{}, ErrInvalidSuperblock
	}

	sb := v.Superblock()
	report := VolumeIntegrityReport{
		Type:                     "XFS",
		FormatVersion:            sb.FormatVersion,
		BlockSize:                sb.BlockSize,
		InodeSize:                sb.InodeSize,
		DirectoryBlockSize:       sb.DirectoryBlockSize,
		RootDirectoryInodeNumber: sb.RootDirectoryInodeNumber,
		AllocationGroupSize:      sb.AllocationGroupSize,
		NumberOfAllocationGroups: sb.NumberOfAllocationGroups,
		NumberOfBlocks:           sb.NumberOfBlocks,
		VolumeLabel:              strings.TrimRight(string(sb.VolumeLabel[:]), "\x00"),
	}

	checked, valid, crcErr := v.verifySuperblockCRC()
	report.SuperblockCRCChecked = checked
	report.SuperblockCRCValid = valid
	if checked && !valid {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_SUPERBLOCK_CRC_MISMATCH", Severity: "high", Message: "v5 superblock CRC32c mismatch"})
		if mode == VerificationModeStrict {
			return report, ErrVerificationFailed
		}
	}
	if crcErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_SUPERBLOCK_CRC_ERROR", Severity: "medium", Message: crcErr.Error()})
		if mode == VerificationModeStrict {
			return report, ErrVerificationFailed
		}
	}

	if sb.BlockSize == 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INVALID_BLOCK_SIZE", Severity: "high", Message: "superblock block size is zero"})
	}
	if sb.InodeSize == 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INVALID_INODE_SIZE", Severity: "high", Message: "superblock inode size is zero"})
	}
	if sb.DirectoryBlockSize == 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INVALID_DIR_BLOCK_SIZE", Severity: "high", Message: "superblock directory block size is zero"})
	}
	if sb.BlockSize != 0 && sb.DirectoryBlockSize%sb.BlockSize != 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "DIR_BLOCK_ALIGNMENT", Severity: "medium", Message: "directory block size is not a multiple of filesystem block size"})
	}
	if _, err := v.GetRootInode(); err != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "ROOT_INODE_OPEN_FAILED", Severity: "high", Message: err.Error(), Inode: sb.RootDirectoryInodeNumber})
	}

	return report, nil
}

// InodeForensicReport builds a structured report for one inode.
func (v *Volume) InodeForensicReport(inodeNumber uint64) (InodeForensicReport, error) {
	return v.inodeForensicReportWithMode(inodeNumber, VerificationModeBestEffort)
}

func (v *Volume) inodeForensicReportWithMode(inodeNumber uint64, mode VerificationMode) (InodeForensicReport, error) {
	if v.IsClosed() {
		return InodeForensicReport{}, ErrVolumeClosed
	}

	inode, err := v.OpenInode(inodeNumber)
	if err != nil {
		return InodeForensicReport{}, err
	}

	typeLabel := "file"
	if inode.IsDirectory() {
		typeLabel = "directory"
	}

	frag, fragErr := v.AnalyzeInodeFragmentation(inodeNumber)
	if fragErr != nil {
		frag = FragmentationReport{InodeNumber: inodeNumber}
	}

	report := InodeForensicReport{
		InodeNumber:           inodeNumber,
		Type:                  typeLabel,
		FileMode:              inode.FileMode,
		ForkType:              inode.ForkType,
		Size:                  inode.Size,
		OwnerID:               inode.OwnerID,
		GroupID:               inode.GroupID,
		NumberOfLinks:         inode.NumberOfLinks,
		AccessTime:            inode.AccessTime(),
		ModificationTime:      inode.ModificationTime(),
		InodeChangeTime:       inode.InodeChangeTime(),
		CreationTime:          inode.CreationTime(),
		DataExtentCount:       len(inode.DataExtents),
		AttributesExtentCount: len(inode.AttributesExtents),
		HasInlineData:         len(inode.InlineData) > 0,
		Fragmentation:         frag,
	}

	for _, extent := range inode.DataExtents {
		isSparse := (extent.RangeFlags & ExtentFlagSparse) != 0
		lengthBytes := uint64(extent.NumberOfBlocks) * uint64(v.ioh.blockSize)
		fragment := InodeFragment{
			LengthBytes:         lengthBytes,
			LogicalBlockNumber:  extent.LogicalBlockNumber,
			PhysicalBlockNumber: extent.PhysicalBlockNumber,
			NumberOfBlocks:      extent.NumberOfBlocks,
			IsSparse:            isSparse,
		}
		if !isSparse && extent.NumberOfBlocks > 0 {
			fragment.StartOffset = extent.PhysicalBlockNumber * uint64(v.ioh.blockSize)
			fragment.EndOffset = fragment.StartOffset + lengthBytes
		}
		report.Fragments = append(report.Fragments, fragment)
	}

	checked, valid, crcErr := verifyInodeCRC(inode)
	if checked && !valid {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_INODE_CRC_MISMATCH", Severity: "high", Message: "v3 inode CRC32c mismatch", Inode: inodeNumber})
		if mode == VerificationModeStrict {
			return InodeForensicReport{}, ErrVerificationFailed
		}
	}
	if crcErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_INODE_CRC_ERROR", Severity: "medium", Message: crcErr.Error(), Inode: inodeNumber})
		if mode == VerificationModeStrict {
			return InodeForensicReport{}, ErrVerificationFailed
		}
	}

	attrs, attrErr := v.ListInodeExtendedAttributes(inodeNumber)
	if attrErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "XATTR_SCAN_FAILED", Severity: "low", Message: attrErr.Error(), Inode: inodeNumber})
	} else {
		names := make([]string, 0, len(attrs))
		for _, attr := range attrs {
			names = append(names, attr.Namespace+"."+attr.Name)
		}
		sort.Strings(names)
		report.ExtendedAttributeNames = names
	}

	if fragErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "FRAGMENTATION_ANALYSIS_FAILED", Severity: "low", Message: fragErr.Error(), Inode: inodeNumber})
	}

	return report, nil
}

// InodeForensicReportByPath resolves a path and reports that inode.
func (v *Volume) InodeForensicReportByPath(path string) (InodeForensicReport, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return InodeForensicReport{}, err
	}
	report, err := v.InodeForensicReport(inodeNumber)
	if err != nil {
		return InodeForensicReport{}, err
	}
	report.Path = path
	return report, nil
}

// DirectoryArtifactReport reports active/deleted/carved records for one directory.
func (v *Volume) DirectoryArtifactReport(inodeNumber uint64) (DirectoryArtifactReport, error) {
	if v.IsClosed() {
		return DirectoryArtifactReport{}, ErrVolumeClosed
	}

	return v.directoryArtifactReportWithMode(inodeNumber, VerificationModeBestEffort)
}

func (v *Volume) directoryArtifactReportWithMode(inodeNumber uint64, mode VerificationMode) (DirectoryArtifactReport, error) {
	mode = normalizeVerificationMode(mode)

	listing, err := v.ScanDirectoryRecordsWithOptions(inodeNumber, DirectoryScanOptions{
		BestEffort: mode == VerificationModeBestEffort,
	})
	if err != nil {
		return DirectoryArtifactReport{}, err
	}

	report := DirectoryArtifactReport{
		InodeNumber: inodeNumber,
		Records:     listing.Records,
		RecordCount: len(listing.Records),
		Anomalies:   listing.Anomalies,
	}
	if listing.Truncated {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{
			Code:     "DIRECTORY_SCAN_TRUNCATED",
			Severity: "medium",
			Inode:    inodeNumber,
			Message:  "directory scan hit a safety cap; records are incomplete",
		})
	}
	for _, record := range listing.Records {
		if record.IsDeleted {
			report.DeletedCount++
		} else {
			report.ActiveCount++
		}
		if record.IsCarved {
			report.CarvedCount++
		}
	}
	return report, nil
}

// DirectoryArtifactReportByPath resolves a path and reports directory artifacts.
func (v *Volume) DirectoryArtifactReportByPath(path string) (DirectoryArtifactReport, error) {
	inodeNumber, err := v.ResolveInodeByPath(path)
	if err != nil {
		return DirectoryArtifactReport{}, err
	}
	report, err := v.DirectoryArtifactReport(inodeNumber)
	if err != nil {
		return DirectoryArtifactReport{}, err
	}
	report.Path = path
	return report, nil
}

// Report builds a combined report from root path "/".
func (v *Volume) Report() (*XFSReport, error) {
	return v.ReportWithOptions(ReportOptions{})
}

// ReportWithOptions builds a combined forensic report.
func (v *Volume) ReportWithOptions(options ReportOptions) (*XFSReport, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}

	mode := normalizeVerificationMode(options.VerificationMode)

	rootPath := options.RootPath
	if rootPath == "" {
		rootPath = "/"
	}

	rootInode, err := v.ResolveInodeByPath(rootPath)
	if err != nil {
		return nil, err
	}

	volumeReport, err := v.volumeIntegrityReportWithMode(mode)
	if err != nil {
		return nil, err
	}

	report := &XFSReport{
		GeneratedAt: time.Now().UTC(),
		RootPath:    rootPath,
		Provenance: ReportProvenance{
			ParserVersion:    Version,
			VerificationMode: mode,
			Coverage: []string{
				"geometry_sanity",
				"v5_superblock_crc32c",
				"v3_inode_crc32c",
			},
			SuperblockCRCChecked: volumeReport.SuperblockCRCChecked,
		},
		Volume: volumeReport,
		Files:  make([]InodeForensicReport, 0, 64),
	}

	type walkItem struct {
		inode uint64
		path  string
	}
	stack := []walkItem{{inode: rootInode, path: rootPath}}
	visited := make(map[uint64]bool, 64)

	for len(stack) > 0 {
		if options.MaxEntries > 0 && len(report.Files) >= options.MaxEntries {
			report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "MAX_ENTRIES_REACHED", Severity: "low", Message: fmt.Sprintf("report entry cap reached (%d)", options.MaxEntries)})
			break
		}

		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if visited[item.inode] {
			continue
		}
		visited[item.inode] = true

		inodeReport, inodeErr := v.inodeForensicReportWithMode(item.inode, mode)
		if inodeErr != nil {
			report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INODE_REPORT_FAILED", Severity: "medium", Message: inodeErr.Error(), Path: item.path, Inode: item.inode})
			if mode == VerificationModeStrict {
				return nil, inodeErr
			}
			continue
		}
		if !report.Provenance.InodeCRCChecked {
			if inodeObj, openErr := v.OpenInode(item.inode); openErr == nil && inodeObj.FormatVersion == 3 {
				report.Provenance.InodeCRCChecked = true
			}
		}
		inodeReport.Path = item.path
		report.Files = append(report.Files, inodeReport)

		if inodeReport.Type != "directory" {
			continue
		}

		if options.IncludeDirectoryArtifacts {
			dirReport, dirErr := v.directoryArtifactReportWithMode(item.inode, mode)
			if dirErr != nil {
				report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "DIRECTORY_ARTIFACT_SCAN_FAILED", Severity: "low", Message: dirErr.Error(), Path: item.path, Inode: item.inode})
			} else {
				dirReport.Path = item.path
				report.DirectoryArtifacts = append(report.DirectoryArtifacts, dirReport)
			}
		}

		entries, listErr := v.ListDirectoryEntries(item.inode)
		if listErr != nil {
			report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "DIRECTORY_LIST_FAILED", Severity: "medium", Message: listErr.Error(), Path: item.path, Inode: item.inode})
			continue
		}

		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Name == entries[j].Name {
				return entries[i].InodeNumber < entries[j].InodeNumber
			}
			return entries[i].Name < entries[j].Name
		})

		for i := len(entries) - 1; i >= 0; i-- {
			entry := entries[i]
			if entry.Name == "." || entry.Name == ".." {
				continue
			}
			nextPath := joinPath(item.path, entry.Name)
			stack = append(stack, walkItem{inode: entry.InodeNumber, path: nextPath})
		}
	}

	sort.Slice(report.Files, func(i, j int) bool {
		if report.Files[i].Path == report.Files[j].Path {
			return report.Files[i].InodeNumber < report.Files[j].InodeNumber
		}
		return report.Files[i].Path < report.Files[j].Path
	})

	sort.Slice(report.DirectoryArtifacts, func(i, j int) bool {
		if report.DirectoryArtifacts[i].Path == report.DirectoryArtifacts[j].Path {
			return report.DirectoryArtifacts[i].InodeNumber < report.DirectoryArtifacts[j].InodeNumber
		}
		return report.DirectoryArtifacts[i].Path < report.DirectoryArtifacts[j].Path
	})

	return report, nil
}

func normalizeVerificationMode(mode VerificationMode) VerificationMode {
	if mode == VerificationModeStrict {
		return VerificationModeStrict
	}
	return VerificationModeBestEffort
}

func (v *Volume) verifySuperblockCRC() (bool, bool, error) {
	if v.sb == nil || v.sb.FormatVersion != 5 {
		return false, false, nil
	}

	buf := make([]byte, superblockSize)
	if err := readAtFull(v.reader, buf, 0); err != nil {
		return true, false, wrapIOError("read", 0, len(buf), err)
	}
	if len(buf) < 228 {
		return true, false, wrapParseError(0, "superblock_crc", ErrInvalidSuperblock)
	}

	expected := binary.BigEndian.Uint32(buf[224:228])
	actual := computeCRC32cWithZeroedField(buf, 224)
	return true, actual == expected, nil
}

func verifyInodeCRC(inode *Inode) (bool, bool, error) {
	if inode == nil || inode.FormatVersion != 3 {
		return false, false, nil
	}
	if len(inode.Raw) < 104 {
		return true, false, wrapParseError(0, "inode_crc", ErrInvalidInode)
	}

	expected := binary.BigEndian.Uint32(inode.Raw[100:104])
	actual := computeCRC32cWithZeroedField(inode.Raw, 100)
	return true, actual == expected, nil
}

func computeCRC32cWithZeroedField(data []byte, checksumOffset int) uint32 {
	working := append([]byte(nil), data...)
	if checksumOffset >= 0 && checksumOffset+4 <= len(working) {
		for i := 0; i < 4; i++ {
			working[checksumOffset+i] = 0
		}
	}
	table := crc32.MakeTable(crc32.Castagnoli)
	return crc32.Checksum(working, table)
}

func joinPath(base, name string) string {
	if base == "/" {
		return "/" + name
	}
	return strings.TrimRight(base, "/") + "/" + name
}
