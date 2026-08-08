package libxfs

// Committed regression fixtures.
//
// The oracle tests in oracle_diff_test.go need Linux, root, xfsprogs and a
// freshly built corpus. That is the right way to establish the truth, but it
// is not something an ordinary `go test` run can do, so the conclusions those
// tests reach would not be defended by CI or by a contributor on another
// platform.
//
// These fixtures close that gap. Each is a metadata-only image produced by
// xfs_metadump from a corpus image that passed the oracle tests, restored,
// gzipped and committed, alongside a manifest of what the oracle said it
// contains. They run everywhere with no environment variable and no tooling.
//
// File data is not preserved by xfs_metadump, so these assert structure --
// paths, inode numbers, kinds, sizes, per-directory counts and on-disk
// formats -- and never file contents. Content is checked against the kernel by
// TestFileContentMatchesOracle on the full corpus.
//
// Regenerate with tools/corpus/mkfixtures.sh after rebuilding the corpus.

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

const (
	fixtureCorpusDir   = "testdata/corpus"
	fixtureWriteEnvVar = "LIBXFS_WRITE_FIXTURES"
)

// fixtureManifest is what a corpus image was independently determined to hold.
type fixtureManifest struct {
	Case       string `json:"case"`
	Generator  string `json:"generator"`
	OracleUsed string `json:"oracle_used"`

	BlockSize          uint64 `json:"block_size"`
	DirectoryBlockSize uint64 `json:"directory_block_size"`
	RootInode          uint64 `json:"root_inode"`

	TotalPaths       int `json:"total_paths"`
	TotalDirectories int `json:"total_directories"`

	// PathSetSHA256 digests the exact set of paths and inode numbers, so any
	// added, missing or misnumbered entry changes it. Per-directory counts
	// below exist to localise a failure that this only detects.
	PathSetSHA256 string `json:"path_set_sha256"`
	// AttributeSetSHA256 additionally covers each object's kind and size. It
	// is empty for images the kernel could not mount, where those are unknown.
	AttributeSetSHA256 string `json:"attribute_set_sha256,omitempty"`

	Directories []fixtureDirectory `json:"directories"`

	// DeletedDirectory and DeletedNames record entries that were removed from
	// this image while it was mounted, so the committed fixture can check what
	// the carver recovers without needing the corpus that produced it.
	DeletedDirectory uint64   `json:"deleted_directory,omitempty"`
	DeletedNames     []string `json:"deleted_names,omitempty"`
}

type fixtureDirectory struct {
	Path         string `json:"path"`
	Inode        uint64 `json:"inode"`
	SourceFormat string `json:"source_format"`
	Entries      int    `json:"entries"`
}

// canonicalPathSet serialises paths and inode numbers in a fixed order.
func canonicalPathSet(records map[string]oracleRecord) string {
	paths := make([]string, 0, len(records))
	for path := range records {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var builder strings.Builder
	for _, path := range paths {
		fmt.Fprintf(&builder, "%s\x00%d\n", path, records[path].Ino)
	}
	return builder.String()
}

// canonicalAttributeSet additionally covers kind and size.
func canonicalAttributeSet(records map[string]oracleRecord) string {
	paths := make([]string, 0, len(records))
	for path := range records {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	var builder strings.Builder
	for _, path := range paths {
		record := records[path]
		fmt.Fprintf(&builder, "%s\x00%d\x00%s\x00%d\n", path, record.Ino, record.Kind, record.Size)
	}
	return builder.String()
}

func digestOf(value string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
}

// TestWriteCorpusFixtureManifests regenerates the committed manifests from a
// live corpus. It is a tool rather than a test, and does nothing unless asked.
//
// It derives every expectation from the oracle, never from libxfs, so that a
// regenerated manifest cannot quietly bless a regression.
func TestWriteCorpusFixtureManifests(t *testing.T) {
	if os.Getenv(fixtureWriteEnvVar) == "" {
		t.Skipf("%s not set; not regenerating committed fixtures", fixtureWriteEnvVar)
	}

	for _, corpus := range loadCorpus(t) {
		target := filepath.Join(fixtureCorpusDir, corpus.Name)
		if _, err := os.Stat(filepath.Join(target, "image.img.gz")); err != nil {
			t.Logf("case %s has no committed image; skipping its manifest", corpus.Name)
			continue
		}

		expected := corpus.expectedPaths()
		manifest := fixtureManifest{
			Case:               corpus.Name,
			Generator:          readGeneratorLine(corpus.Dir),
			OracleUsed:         "kernel mount and xfs_db",
			BlockSize:          corpus.BlockSize,
			DirectoryBlockSize: corpus.DirectoryBlockSize,
			RootInode:          corpus.RootInode,
			TotalPaths:         len(expected),
			TotalDirectories:   len(corpus.Directories),
			PathSetSHA256:      digestOf(canonicalPathSet(expected)),
		}
		if corpus.attributesKnown() {
			manifest.AttributeSetSHA256 = digestOf(canonicalAttributeSet(expected))
		} else {
			manifest.OracleUsed = "xfs_db only (image is not mountable on this kernel)"
		}

		counts := map[string]int{}
		for path := range expected {
			if path != "/" {
				counts[parentPath(path)]++
			}
		}
		for inode, directory := range corpus.Directories {
			manifest.Directories = append(manifest.Directories, fixtureDirectory{
				Path:         directory.Path,
				Inode:        inode,
				SourceFormat: directory.IndexFormat,
				Entries:      counts[directory.Path],
			})
		}
		sort.Slice(manifest.Directories, func(i, j int) bool {
			return manifest.Directories[i].Path < manifest.Directories[j].Path
		})

		if len(corpus.DeletedNames) > 0 {
			manifest.DeletedDirectory = corpus.NcheckPaths["/target"]
			manifest.DeletedNames = append([]string(nil), corpus.DeletedNames...)
			sort.Strings(manifest.DeletedNames)
		}

		encoded, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			t.Fatalf("case %s: %v", corpus.Name, err)
		}
		if err := os.WriteFile(filepath.Join(target, "manifest.json"), append(encoded, '\n'), 0o644); err != nil {
			t.Fatalf("case %s: %v", corpus.Name, err)
		}
		t.Logf("wrote %s/manifest.json (%d paths, %d directories)",
			target, manifest.TotalPaths, manifest.TotalDirectories)
	}
}

func readGeneratorLine(caseDir string) string {
	data, err := os.ReadFile(filepath.Join(caseDir, "geometry.txt"))
	if err != nil {
		return "unknown"
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "# mkfs.xfs: ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# mkfs.xfs: "))
		}
	}
	return "unknown"
}

// TestCommittedCorpusFixtures walks each committed image and checks it against
// its manifest. This is the regression net: it needs no corpus, no root, no
// Linux and no xfsprogs.
func TestCommittedCorpusFixtures(t *testing.T) {
	entries, err := os.ReadDir(fixtureCorpusDir)
	if os.IsNotExist(err) {
		t.Skipf("no committed fixtures at %s; build them with tools/corpus/mkfixtures.sh", fixtureCorpusDir)
	}
	if err != nil {
		t.Fatalf("reading %s: %v", fixtureCorpusDir, err)
	}

	// The manifests are committed; the images they describe are not, because
	// they are megabytes of binary each. A checkout therefore has the
	// expectations but not the evidence until the images are rebuilt, and a
	// case in that state is skipped rather than failed.
	found, missing := 0, 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		caseDir := filepath.Join(fixtureCorpusDir, entry.Name())
		if _, err := os.Stat(filepath.Join(caseDir, "manifest.json")); err != nil {
			continue
		}
		if _, err := os.Stat(filepath.Join(caseDir, "image.img.gz")); err != nil {
			missing++
			continue
		}
		found++
		t.Run(entry.Name(), func(t *testing.T) {
			runCommittedFixture(t, caseDir)
		})
	}

	if found == 0 {
		t.Skipf("%d fixture manifests found but no images to check them against; "+
			"rebuild with tools/corpus/mkcorpus.sh, mkoracle.sh and mkfixtures.sh", missing)
	}
	if missing > 0 {
		t.Logf("checked %d fixtures; %d have a manifest but no image and were skipped", found, missing)
	}
}

func runCommittedFixture(t *testing.T, caseDir string) {
	t.Helper()

	manifestBytes, err := os.ReadFile(filepath.Join(caseDir, "manifest.json"))
	if err != nil {
		t.Fatalf("reading manifest: %v", err)
	}
	var manifest fixtureManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatalf("parsing manifest: %v", err)
	}

	image := decompressFixtureImage(t, filepath.Join(caseDir, "image.img.gz"))
	volume, err := Open(bytes.NewReader(image))
	if err != nil {
		t.Fatalf("opening fixture image: %v", err)
	}
	t.Cleanup(func() { _ = volume.Close() })

	result := walkVolume(t, volume)

	// Rebuild the same canonical forms the manifest was made from, this time
	// from the walk, and compare digests.
	walked := make(map[string]oracleRecord, len(result.Entries)+1)
	rootInode, err := volume.ResolveInodeByPath("/")
	if err != nil {
		t.Fatalf("resolving root: %v", err)
	}
	rootRecord := oracleRecord{Path: "/", Ino: rootInode, Kind: "dir"}
	if root, err := volume.OpenInode(rootInode); err == nil {
		rootRecord.Size = root.Size
	}
	walked["/"] = rootRecord
	for path, entry := range result.Entries {
		walked[path] = oracleRecord{Path: path, Ino: entry.Ino, Kind: entry.Kind, Size: entry.Size}
	}

	if len(walked) != manifest.TotalPaths {
		t.Errorf("walk found %d paths, the manifest records %d", len(walked), manifest.TotalPaths)
	}
	if digest := digestOf(canonicalPathSet(walked)); digest != manifest.PathSetSHA256 {
		t.Errorf("path set digest %s does not match the manifest's %s", digest, manifest.PathSetSHA256)
	}
	if manifest.AttributeSetSHA256 != "" {
		if digest := digestOf(canonicalAttributeSet(walked)); digest != manifest.AttributeSetSHA256 {
			t.Errorf("path, kind and size digest %s does not match the manifest's %s",
				digest, manifest.AttributeSetSHA256)
		}
	}

	for _, failure := range result.Failures {
		t.Errorf("directory %s (inode %d) failed to scan: %v", failure.Path, failure.Ino, failure.Err)
	}

	// Per-directory counts and formats, which localise any digest mismatch.
	perDirectory := map[string]int{}
	for path := range walked {
		if path != "/" {
			perDirectory[parentPath(path)]++
		}
	}
	for _, directory := range manifest.Directories {
		if got := perDirectory[directory.Path]; got != directory.Entries {
			t.Errorf("directory %s (%s format): walk found %d entries, the manifest records %d",
				directory.Path, directory.SourceFormat, got, directory.Entries)
		}
		listing, err := volume.ListDirectoryEntriesReport(directory.Inode)
		if err != nil {
			t.Errorf("directory %s (inode %d): %v", directory.Path, directory.Inode, err)
			continue
		}
		if listing.SourceFormat != directory.SourceFormat {
			t.Errorf("directory %s: libxfs reports on-disk format %q, the manifest records %q",
				directory.Path, listing.SourceFormat, directory.SourceFormat)
		}
	}

	if len(manifest.DeletedNames) > 0 {
		checkFixtureDeletedEntries(t, volume, manifest)
	}

	t.Logf("%d paths across %d directories verified against %s",
		manifest.TotalPaths, manifest.TotalDirectories, manifest.Generator)
}

// checkFixtureDeletedEntries repeats the deleted-entry checks on a committed
// image, so the carver's soundness and its recall floor are defended without a
// corpus, root or Linux.
func checkFixtureDeletedEntries(t *testing.T, volume *Volume, manifest fixtureManifest) {
	t.Helper()

	listing, err := volume.ScanDirectoryRecordsWithOptions(manifest.DeletedDirectory,
		DirectoryScanOptions{BestEffort: true})
	if err != nil {
		t.Fatalf("scanning the directory entries were deleted from: %v", err)
	}

	deleted := map[string]bool{}
	for _, name := range manifest.DeletedNames {
		deleted[name] = true
	}

	recovered := 0
	seen := map[string]bool{}
	for _, record := range listing.Records {
		switch record.Kind {
		case RecordKindActive:
			// Soundness: a deleted name must never be reported as live.
			if deleted[record.Name] {
				t.Errorf("deleted entry %q is reported as a live directory entry", record.Name)
			}
		case RecordKindCarved:
			if !record.IsProbabilistic() || record.IsVerified() {
				t.Errorf("carved record %q is not presented as a candidate: %+v", record.Name, record)
			}
			if deleted[record.Name] && !seen[record.Name] {
				seen[record.Name] = true
				recovered++
			}
		}
	}

	if recovered < deletedEntryRecallFloor {
		t.Errorf("recovered %d of %d deleted entries, below the pinned floor of %d",
			recovered, len(deleted), deletedEntryRecallFloor)
	}
	t.Logf("recovered %d of %d deleted entries (floor %d)", recovered, len(deleted), deletedEntryRecallFloor)
}

// decompressFixtureImage expands a committed image into memory.
//
// The images are mostly zeros, which is why they compress to a few hundred
// kilobytes, and why holding one expanded is the simplest thing that works.
func decompressFixtureImage(t *testing.T, path string) []byte {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer file.Close()

	reader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	defer reader.Close()

	image, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("decompressing %s: %v", path, err)
	}
	return image
}
