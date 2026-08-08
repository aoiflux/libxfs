package libxfs

import (
	"context"
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
	// Concurrency optionally analyses discovered inodes in parallel. The
	// zero value is sequential. Output is identical regardless of the
	// worker count.
	Concurrency Concurrency
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
	// IsSparse marks a fragment that reads back as zeros, whether because it
	// is an unmapped hole or because it was preallocated and never written.
	IsSparse bool `json:"is_sparse"`
	// IsUnwritten distinguishes the second case: blocks were reserved on disk
	// and never written to. Unlike a hole, PhysicalBlockNumber names real
	// blocks whose prior contents may still be recoverable from the medium.
	IsUnwritten bool `json:"is_unwritten"`
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
		if file.Type == InodeTypeDirectory {
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
	fmt.Fprintf(&b, "Verification mode: %s\n", r.Provenance.VerificationMode)
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
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_SUPERBLOCK_CRC_MISMATCH", Severity: SeverityHigh, Message: "v5 superblock CRC32c mismatch"})
		if mode == VerificationModeStrict {
			return report, ErrVerificationFailed
		}
	}
	if crcErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_SUPERBLOCK_CRC_ERROR", Severity: SeverityMedium, Message: crcErr.Error()})
		if mode == VerificationModeStrict {
			return report, ErrVerificationFailed
		}
	}

	if sb.BlockSize == 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INVALID_BLOCK_SIZE", Severity: SeverityHigh, Message: "superblock block size is zero"})
	}
	if sb.InodeSize == 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INVALID_INODE_SIZE", Severity: SeverityHigh, Message: "superblock inode size is zero"})
	}
	if sb.DirectoryBlockSize == 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "INVALID_DIR_BLOCK_SIZE", Severity: SeverityHigh, Message: "superblock directory block size is zero"})
	}
	if sb.BlockSize != 0 && sb.DirectoryBlockSize%sb.BlockSize != 0 {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "DIR_BLOCK_ALIGNMENT", Severity: SeverityMedium, Message: "directory block size is not a multiple of filesystem block size"})
	}
	if _, err := v.GetRootInode(); err != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "ROOT_INODE_OPEN_FAILED", Severity: SeverityHigh, Message: err.Error(), Inode: sb.RootDirectoryInodeNumber})
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

	typeLabel := InodeTypeFile
	if inode.IsDirectory() {
		typeLabel = InodeTypeDirectory
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
			IsUnwritten:         (extent.RangeFlags & ExtentFlagUnwritten) != 0,
		}
		// StartOffset is a byte offset into the volume, so the allocation
		// group packed into the block number has to be resolved first.
		// Scaling the raw block number by the block size names a location
		// that is not where the data lives.
		//
		// An unwritten fragment gets an offset even though it reads as zeros:
		// it occupies real blocks, and where they are is exactly what makes it
		// worth examining. A hole has no location to report.
		if (!isSparse || fragment.IsUnwritten) && extent.NumberOfBlocks > 0 {
			if start, err := v.fileSystemBlockOffset(extent.PhysicalBlockNumber); err == nil {
				fragment.StartOffset = uint64(start)
				fragment.EndOffset = fragment.StartOffset + lengthBytes
			}
		}
		report.Fragments = append(report.Fragments, fragment)
	}

	checked, valid, crcErr := verifyInodeCRC(inode)
	if checked && !valid {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_INODE_CRC_MISMATCH", Severity: SeverityHigh, Message: "v3 inode CRC32c mismatch", Inode: inodeNumber})
		if mode == VerificationModeStrict {
			return InodeForensicReport{}, ErrVerificationFailed
		}
	}
	if crcErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "VERIFY_INODE_CRC_ERROR", Severity: SeverityMedium, Message: crcErr.Error(), Inode: inodeNumber})
		if mode == VerificationModeStrict {
			return InodeForensicReport{}, ErrVerificationFailed
		}
	}

	attrs, attrErr := v.ListInodeExtendedAttributes(inodeNumber)
	if attrErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "XATTR_SCAN_FAILED", Severity: SeverityLow, Message: attrErr.Error(), Inode: inodeNumber})
	} else {
		names := make([]string, 0, len(attrs))
		for _, attr := range attrs {
			names = append(names, attr.Namespace+"."+attr.Name)
		}
		sort.Strings(names)
		report.ExtendedAttributeNames = names
	}

	if fragErr != nil {
		report.Anomalies = append(report.Anomalies, ReportAnomaly{Code: "FRAGMENTATION_ANALYSIS_FAILED", Severity: SeverityLow, Message: fragErr.Error(), Inode: inodeNumber})
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
			Severity: SeverityMedium,
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
//
// It is equivalent to ReportWithContext with a background context.
func (v *Volume) ReportWithOptions(options ReportOptions) (*XFSReport, error) {
	return v.ReportWithContext(context.Background(), options)
}

// ReportWithContext builds a combined forensic report, honouring context
// cancellation.
//
// Discovery of the inode set is sequential, because a directory must be read
// before its children are known. Per-inode analysis then runs across the pool
// configured by options.Concurrency. The result does not depend on the worker
// count: entries are stored by position and sorted before returning.
func (v *Volume) ReportWithContext(ctx context.Context, options ReportOptions) (*XFSReport, error) {
	if v.IsClosed() {
		return nil, ErrVolumeClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
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
			VerificationMode: mode,
			Coverage: []string{
				"geometry_sanity",
				"v5_superblock_crc32c",
				"v3_inode_crc32c",
			},
			SuperblockCRCChecked: volumeReport.SuperblockCRCChecked,
		},
		Volume: volumeReport,
		Files:  make([]InodeForensicReport, 0, reportInitialCapacity),
	}

	discovered, discoveryAnomalies := v.discoverReportInodes(ctx, rootInode, rootPath, options)
	report.Anomalies = append(report.Anomalies, discoveryAnomalies...)

	inodeReports := make([]InodeForensicReport, len(discovered))
	artifactReports := make([]*DirectoryArtifactReport, len(discovered))
	taskAnomalies := make([][]ReportAnomaly, len(discovered))

	analyse := func(taskCtx context.Context, index int) error {
		if err := taskCtx.Err(); err != nil {
			return err
		}
		item := discovered[index]

		inodeReport, inodeErr := v.inodeForensicReportWithMode(item.inode, mode)
		if inodeErr != nil {
			if mode == VerificationModeStrict {
				return inodeErr
			}
			taskAnomalies[index] = append(taskAnomalies[index], ReportAnomaly{
				Code: "INODE_REPORT_FAILED", Severity: SeverityMedium,
				Message: inodeErr.Error(), Path: item.path, Inode: item.inode,
			})
			return nil
		}
		inodeReport.Path = item.path
		inodeReports[index] = inodeReport

		if !item.isDirectory || !options.IncludeDirectoryArtifacts {
			return nil
		}

		dirReport, dirErr := v.directoryArtifactReportWithMode(item.inode, mode)
		if dirErr != nil {
			taskAnomalies[index] = append(taskAnomalies[index], ReportAnomaly{
				Code: "DIRECTORY_ARTIFACT_SCAN_FAILED", Severity: SeverityLow,
				Message: dirErr.Error(), Path: item.path, Inode: item.inode,
			})
			return nil
		}
		dirReport.Path = item.path
		artifactReports[index] = &dirReport
		return nil
	}

	if err := runBounded(ctx, options.Concurrency, len(discovered), analyse); err != nil {
		return nil, err
	}

	// Collect in discovery order so the result is independent of scheduling.
	for index, item := range discovered {
		report.Anomalies = append(report.Anomalies, taskAnomalies[index]...)
		if inodeReports[index].InodeNumber == 0 {
			continue
		}
		report.Files = append(report.Files, inodeReports[index])
		if artifactReports[index] != nil {
			report.DirectoryArtifacts = append(report.DirectoryArtifacts, *artifactReports[index])
		}
		if !report.Provenance.InodeCRCChecked {
			if inodeObj, openErr := v.OpenInode(item.inode); openErr == nil &&
				inodeObj.FormatVersion == inodeVersion3 {
				report.Provenance.InodeCRCChecked = true
			}
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

// reportWalkItem is one inode discovered during the sequential tree walk.
type reportWalkItem struct {
	inode       uint64
	path        string
	isDirectory bool
}

// discoverReportInodes walks the tree from root and returns the inodes to
// analyse, in a deterministic order.
//
// This phase stays sequential by necessity: a directory has to be read before
// its children are known. It performs no per-inode analysis, so it is cheap
// relative to the parallel phase that follows.
func (v *Volume) discoverReportInodes(ctx context.Context, rootInode uint64, rootPath string,
	options ReportOptions) ([]reportWalkItem, []ReportAnomaly) {

	var discovered []reportWalkItem
	var anomalies []ReportAnomaly

	stack := []reportWalkItem{{inode: rootInode, path: rootPath}}
	visited := make(map[uint64]bool, reportInitialCapacity)

	for len(stack) > 0 {
		if err := ctx.Err(); err != nil {
			return discovered, anomalies
		}
		if options.MaxEntries > 0 && len(discovered) >= options.MaxEntries {
			anomalies = append(anomalies, ReportAnomaly{
				Code: "MAX_ENTRIES_REACHED", Severity: SeverityLow,
				Message: fmt.Sprintf("report entry cap reached (%d)", options.MaxEntries),
			})
			break
		}

		item := stack[len(stack)-1]
		stack = stack[:len(stack)-1]

		if visited[item.inode] {
			continue
		}
		visited[item.inode] = true

		inode, err := v.OpenInode(item.inode)
		if err != nil {
			anomalies = append(anomalies, ReportAnomaly{
				Code: "INODE_OPEN_FAILED", Severity: SeverityMedium,
				Message: err.Error(), Path: item.path, Inode: item.inode,
			})
			continue
		}
		item.isDirectory = inode.IsDirectory()
		discovered = append(discovered, item)

		if !item.isDirectory {
			continue
		}

		entries, listErr := v.ListDirectoryEntries(item.inode)
		if listErr != nil {
			anomalies = append(anomalies, ReportAnomaly{
				Code: "DIRECTORY_LIST_FAILED", Severity: SeverityMedium,
				Message: listErr.Error(), Path: item.path, Inode: item.inode,
			})
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
			stack = append(stack, reportWalkItem{
				inode: entry.InodeNumber,
				path:  joinPath(item.path, entry.Name),
			})
		}
	}
	return discovered, anomalies
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
	if err := v.readAt(buf, 0); err != nil {
		return true, false, wrapIOError("read", 0, len(buf), err)
	}
	if len(buf) < sbOffsetChecksum+checksumSize {
		return true, false, wrapParseError(0, "superblock_crc", ErrInvalidSuperblock)
	}

	expected := binary.BigEndian.Uint32(buf[sbOffsetChecksum : sbOffsetChecksum+checksumSize])
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
