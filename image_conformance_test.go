package libxfs

import (
	"os"
	"testing"
	"time"
)

// Conformance tests against a real XFS image.
//
// Synthetic fixtures can only prove that the parser agrees with the author's
// reading of the format. These tests check it against a filesystem produced by
// mkfs.xfs, which is the only way to catch a misunderstanding shared by both
// the parser and its fixtures.
//
// Set LIBXFS_TEST_IMAGE to the path of an XFS image to enable them:
//
//	LIBXFS_TEST_IMAGE=/path/to/xfs.dd go test ./...
//
// They are skipped when the variable is unset so the default test run stays
// self-contained.

func openConformanceImage(t *testing.T) *Volume {
	t.Helper()

	path := os.Getenv("LIBXFS_TEST_IMAGE")
	if path == "" {
		t.Skip("LIBXFS_TEST_IMAGE not set; skipping real-image conformance tests")
	}

	volume, err := OpenVolumeFromPath(path)
	if err != nil {
		t.Fatalf("failed to open %s: %v", path, err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	return volume
}

// walkImage visits every reachable directory, returning the traversal so that
// individual tests can assert over it.
type imageDirectory struct {
	inode  uint64
	path   string
	parent uint64
}

func walkImageDirectories(t *testing.T, volume *Volume) []imageDirectory {
	t.Helper()

	root := volume.Superblock().RootDirectoryInodeNumber
	pending := []imageDirectory{{inode: root, path: "/", parent: root}}
	visited := map[uint64]bool{}
	var directories []imageDirectory

	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[current.inode] {
			continue
		}
		visited[current.inode] = true
		directories = append(directories, current)

		entries, err := volume.ListDirectoryEntries(current.inode)
		if err != nil {
			t.Errorf("listing %s (inode %d) failed: %v", current.path, current.inode, err)
			continue
		}
		for _, entry := range entries {
			if entry.FileType != DirEntryFileTypeDirectory {
				continue
			}
			childPath := current.path
			if childPath != "/" {
				childPath += "/"
			}
			pending = append(pending, imageDirectory{
				inode:  entry.InodeNumber,
				path:   childPath + entry.Name,
				parent: current.inode,
			})
		}
	}
	return directories
}

// TestImageListsEveryDirectory is the headline conformance check: a real
// filesystem must be traversable end to end without a single parse failure.
//
// It is what caught the v5 directory header size bug, where a block-format
// directory failed to parse and its files vanished from the listing entirely.
func TestImageListsEveryDirectory(t *testing.T) {
	volume := openConformanceImage(t)
	directories := walkImageDirectories(t, volume)

	if len(directories) == 0 {
		t.Fatal("no directories reached from the root")
	}
	t.Logf("traversed %d directories", len(directories))
}

// TestImageParentLinksAgreeWithTraversal cross-checks every directory's ".."
// against the path actually used to reach it. The two are derived from
// completely different on-disk structures, so agreement is real evidence.
func TestImageParentLinksAgreeWithTraversal(t *testing.T) {
	volume := openConformanceImage(t)

	for _, directory := range walkImageDirectories(t, volume) {
		parent, err := volume.DirectoryParentInode(directory.inode)
		if err != nil {
			t.Errorf("%s: DirectoryParentInode failed: %v", directory.path, err)
			continue
		}
		if parent != directory.parent {
			t.Errorf("%s: parent link says %d, traversal says %d",
				directory.path, parent, directory.parent)
		}
	}
}

// TestImageFileDataMatchesRecordedSize reads every regular file in full and
// checks the byte count against the size in its inode, exercising extent
// mapping across the whole image.
func TestImageFileDataMatchesRecordedSize(t *testing.T) {
	volume := openConformanceImage(t)

	files, totalBytes := 0, 0
	for _, directory := range walkImageDirectories(t, volume) {
		entries, err := volume.ListDirectoryEntries(directory.inode)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.FileType != DirEntryFileTypeRegularFile {
				continue
			}
			inode, err := volume.OpenInode(entry.InodeNumber)
			if err != nil {
				t.Errorf("%s/%s: OpenInode failed: %v", directory.path, entry.Name, err)
				continue
			}
			data, err := volume.ReadFileData(entry.InodeNumber)
			if err != nil {
				t.Errorf("%s/%s: ReadFileData failed: %v", directory.path, entry.Name, err)
				continue
			}
			if uint64(len(data)) != inode.Size {
				t.Errorf("%s/%s: read %d bytes, inode records %d",
					directory.path, entry.Name, len(data), inode.Size)
			}
			files++
			totalBytes += len(data)
		}
	}

	if files == 0 {
		t.Fatal("no regular files found")
	}
	t.Logf("read %d files, %d bytes", files, totalBytes)
}

// TestImageTimestampsArePlausible catches timestamp decoding that produces
// self-consistent but wrong values.
//
// A bigtime timestamp read as a legacy pair lands around 1998 — a date that
// looks entirely believable in isolation. Bounding against the era in which
// XFS v5 filesystems can exist is what makes the error visible.
func TestImageTimestampsArePlausible(t *testing.T) {
	volume := openConformanceImage(t)

	// XFS v5 did not exist before 2013, so nothing on such an image can
	// legitimately predate it by default.
	earliest := time.Date(2013, time.January, 1, 0, 0, 0, 0, time.UTC)
	latest := time.Now().UTC().Add(24 * time.Hour)
	usesV5 := volume.Superblock().FormatVersion == 5

	checked := 0
	for _, directory := range walkImageDirectories(t, volume) {
		entries, err := volume.ListDirectoryEntries(directory.inode)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			inode, err := volume.OpenInode(entry.InodeNumber)
			if err != nil {
				continue
			}
			for _, stamp := range []struct {
				name  string
				value time.Time
			}{
				{"access", inode.AccessTime()},
				{"modification", inode.ModificationTime()},
				{"change", inode.InodeChangeTime()},
				{"creation", inode.CreationTime()},
			} {
				if stamp.value.IsZero() {
					continue
				}
				checked++
				if stamp.value.After(latest) {
					t.Errorf("%s/%s: %s time %s is in the future",
						directory.path, entry.Name, stamp.name, stamp.value)
				}
				if usesV5 && stamp.value.Before(earliest) {
					t.Errorf("%s/%s: %s time %s predates XFS v5; timestamps are likely being decoded with the wrong encoding",
						directory.path, entry.Name, stamp.name, stamp.value)
				}
			}
		}
	}

	if checked == 0 {
		t.Fatal("no timestamps checked")
	}
	t.Logf("checked %d timestamps (bigtime=%v)", checked, volume.Superblock().HasBigTimestamps())
}

// TestImageExtendedAttributeNamespaces checks that attributes are reported
// under the namespace names Linux uses, since downstream tooling matches on
// strings such as "security.selinux".
func TestImageExtendedAttributeNamespaces(t *testing.T) {
	volume := openConformanceImage(t)

	valid := map[string]bool{"user": true, "trusted": true, "security": true}
	found := 0

	for _, directory := range walkImageDirectories(t, volume) {
		entries, err := volume.ListDirectoryEntries(directory.inode)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			attributes, err := volume.ListInodeExtendedAttributes(entry.InodeNumber)
			if err != nil {
				t.Errorf("%s/%s: ListInodeExtendedAttributes failed: %v",
					directory.path, entry.Name, err)
				continue
			}
			for _, attribute := range attributes {
				found++
				if !valid[attribute.Namespace] {
					t.Errorf("%s/%s: unexpected xattr namespace %q (name %q)",
						directory.path, entry.Name, attribute.Namespace, attribute.Name)
				}
			}
		}
	}
	t.Logf("checked %d extended attributes", found)
}

// TestImageDirectoryRecordsAreLabelled checks that every record a scan returns
// is self-describing, so a consumer never has to guess whether it is fact.
func TestImageDirectoryRecordsAreLabelled(t *testing.T) {
	volume := openConformanceImage(t)

	active, free, carved := 0, 0, 0
	for _, directory := range walkImageDirectories(t, volume) {
		listing, err := volume.ScanDirectoryRecordsWithOptions(directory.inode, DirectoryScanOptions{})
		if err != nil {
			t.Errorf("%s: scan failed: %v", directory.path, err)
			continue
		}
		for _, record := range listing.Records {
			switch record.Kind {
			case RecordKindActive:
				active++
				if record.Name == "" || record.InodeNumber == 0 {
					t.Errorf("%s: active record without an identity: %+v", directory.path, record)
				}
			case RecordKindFreeSlot:
				free++
				if record.Name != "" || record.InodeNumber != 0 {
					t.Errorf("%s: free slot carries an identity: %+v", directory.path, record)
				}
			case RecordKindCarved:
				carved++
				if !record.IsProbabilistic() {
					t.Errorf("%s: carved record not marked probabilistic: %+v", directory.path, record)
				}
			default:
				t.Errorf("%s: record with unknown kind %q: %+v", directory.path, record.Kind, record)
			}
		}
	}
	t.Logf("records: active=%d free=%d carved=%d", active, free, carved)
}

// TestImageReportGeneration exercises the report surface end to end.
func TestImageReportGeneration(t *testing.T) {
	volume := openConformanceImage(t)

	report, err := volume.ReportWithOptions(ReportOptions{
		IncludeDirectoryArtifacts: true,
		VerificationMode:          VerificationModeBestEffort,
	})
	if err != nil {
		t.Fatalf("ReportWithOptions failed: %v", err)
	}
	if len(report.Files) == 0 {
		t.Fatal("report contains no files")
	}
	if report.Summary() == "" {
		t.Fatal("report summary is empty")
	}
	for _, anomaly := range report.Anomalies {
		t.Errorf("unexpected report anomaly on a healthy image: %+v", anomaly)
	}
	t.Logf("reported %d inodes, %d directory artifacts",
		len(report.Files), len(report.DirectoryArtifacts))
}
