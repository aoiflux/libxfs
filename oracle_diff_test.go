package libxfs

// Differential tests against real XFS images and externally produced ground
// truth.
//
// The question these answer is not "does the parser agree with itself" but
// "does the walk find everything the filesystem contains". That can only be
// settled against an oracle that shares no code with the library, so the
// corpus is built by mkfs.xfs and described by the Linux kernel and xfs_db.
//
// Build a corpus and point the tests at it:
//
//	sudo tools/corpus/mkcorpus.sh
//	sudo tools/corpus/mkoracle.sh
//	LIBXFS_CORPUS=/var/tmp/libxfs-corpus go test -run Oracle -v ./...

import (
	"crypto/sha256"
	"fmt"
	"sort"
	"strings"
	"testing"
)

// walkedEntry is one object as libxfs found it.
type walkedEntry struct {
	Path      string
	Name      string
	Ino       uint64
	Kind      string
	Size      uint64
	ParentIno uint64
	// FileTypeByte is the entry's ftype, which is absent on filesystems
	// without the feature and must not be relied on for classification.
	FileTypeByte uint8
}

// walkedDirectory records how one directory scan went, so that a shortfall can
// be attributed to a directory rather than just counted.
type walkedDirectory struct {
	Path         string
	Ino          uint64
	Err          error
	Format       string
	Truncated    bool
	BlocksRead   uint64
	AnomalyCount int
	ActiveCount  int
}

// walkResult is a full recursive walk performed exactly the way a consumer
// does it: resolve the root by path, then scan records directory by directory.
type walkResult struct {
	Entries     map[string]walkedEntry
	Directories []walkedDirectory
	Failures    []walkedDirectory
}

func joinWalkPath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

func kindFromFileMode(mode uint16) string {
	switch mode & fileModeTypeMask {
	case FileTypeFIFO:
		return "fifo"
	case FileTypeCharacterDevice:
		return "chardev"
	case FileTypeDirectory:
		return "dir"
	case FileTypeBlockDevice:
		return "blockdev"
	case FileTypeRegularFile:
		return "reg"
	case FileTypeSymbolicLink:
		return "symlink"
	case FileTypeSocket:
		return "socket"
	default:
		return fmt.Sprintf("mode_%04x", mode&fileModeTypeMask)
	}
}

// walkVolume mirrors how a forensic consumer drives the library: resolve "/",
// scan records, recurse into anything that turns out to be a directory. It
// deliberately does not use best-effort options, because the point is to
// measure what the default path yields.
func walkVolume(t *testing.T, volume *Volume) walkResult {
	t.Helper()

	result := walkResult{Entries: map[string]walkedEntry{}}

	rootInode, err := volume.ResolveInodeByPath("/")
	if err != nil {
		t.Fatalf("resolving root: %v", err)
	}

	type pending struct {
		ino  uint64
		path string
	}
	queue := []pending{{ino: rootInode, path: "/"}}
	visited := map[uint64]bool{rootInode: true}

	for len(queue) > 0 {
		current := queue[len(queue)-1]
		queue = queue[:len(queue)-1]

		listing, scanErr := volume.ScanDirectoryRecordsWithOptions(current.ino, DirectoryScanOptions{})
		record := walkedDirectory{
			Path:         current.path,
			Ino:          current.ino,
			Err:          scanErr,
			Format:       listing.Format,
			Truncated:    listing.Truncated,
			BlocksRead:   listing.BlocksScanned,
			AnomalyCount: len(listing.Anomalies),
		}
		if scanErr != nil {
			result.Directories = append(result.Directories, record)
			result.Failures = append(result.Failures, record)
			continue
		}

		for _, entry := range listing.Records {
			if entry.Kind != RecordKindActive {
				continue
			}
			if entry.Name == "." || entry.Name == ".." {
				continue
			}
			record.ActiveCount++

			childPath := joinWalkPath(current.path, entry.Name)
			child := walkedEntry{
				Path:         childPath,
				Name:         entry.Name,
				Ino:          entry.InodeNumber,
				ParentIno:    current.ino,
				FileTypeByte: entry.FileType,
				Kind:         "unreadable",
			}

			// Classification comes from the inode, not from the entry's ftype
			// byte: ftype is absent on filesystems without the feature, and a
			// walk that depends on it would silently stop recursing there.
			inode, inodeErr := volume.OpenInode(entry.InodeNumber)
			if inodeErr == nil {
				child.Kind = kindFromFileMode(inode.FileMode)
				child.Size = inode.Size
			}
			result.Entries[childPath] = child

			if inodeErr == nil && inode.IsDirectory() && !visited[entry.InodeNumber] {
				visited[entry.InodeNumber] = true
				queue = append(queue, pending{ino: entry.InodeNumber, path: childPath})
			}
		}
		result.Directories = append(result.Directories, record)
	}
	return result
}

func openCorpusVolume(t *testing.T, corpus corpusCase) *Volume {
	t.Helper()

	volume, err := OpenVolumeFromPath(corpus.ImagePath)
	if err != nil {
		t.Fatalf("opening %s: %v", corpus.ImagePath, err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	return volume
}

// TestOracleCrossValidation checks the two oracles against each other before
// either is used to judge libxfs.
//
// An oracle that has not been cross-checked is just a second opinion. The
// kernel and xfs_db are separate implementations, so exact agreement on the
// path set is real evidence that the ground truth is correct.
func TestOracleCrossValidation(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		t.Run(corpus.Name, func(t *testing.T) {
			if !corpus.hasMountOracle() {
				t.Skipf("no mount oracle for %s; cross validation needs both", corpus.Name)
			}

			// xfs_db names each inode once, so a hardlinked file appears under
			// only one of its paths. That makes ncheck a subset of the kernel's
			// path set, and the check has to be stated as such rather than as
			// equality, or every hardlink reads as a disagreement.
			ncheckInodes := map[uint64]string{}
			for path, inode := range corpus.NcheckPaths {
				ncheckInodes[inode] = path
			}

			aliases := 0
			for path, record := range corpus.MountOracle {
				inode, ok := corpus.NcheckPaths[path]
				if ok {
					if inode != record.Ino {
						t.Errorf("path %q: kernel reports inode %d, xfs_db reports %d",
							path, record.Ino, inode)
					}
					continue
				}
				named, known := ncheckInodes[record.Ino]
				if !known {
					t.Errorf("path %q (inode %d) present in the kernel walk but its inode is absent from xfs_db ncheck",
						path, record.Ino)
					continue
				}
				if record.Nlink < 2 {
					t.Errorf("path %q (inode %d) is absent from xfs_db ncheck but has link count %d, so it is not a hardlink alias of %q",
						path, record.Ino, record.Nlink, named)
					continue
				}
				aliases++
			}

			for path, inode := range corpus.NcheckPaths {
				record, ok := corpus.MountOracle[path]
				if !ok {
					t.Errorf("path %q present in xfs_db ncheck but absent from the kernel walk", path)
					continue
				}
				if record.Ino != inode {
					t.Errorf("path %q: xfs_db reports inode %d, kernel reports %d", path, inode, record.Ino)
				}
			}
			t.Logf("%d paths agreed between the kernel and xfs_db (%d hardlink aliases named only by the kernel)",
				len(corpus.NcheckPaths), aliases)
		})
	}
}

// TestCorpusDirectoryFormatCoverage proves the corpus actually spans the
// format space it claims to.
//
// Directory index format and data-fork mapping format are independent axes. A
// corpus covering only some cells does not establish completeness across
// formats, however many images it contains, so the matrix is asserted rather
// than assumed.
func TestCorpusDirectoryFormatCoverage(t *testing.T) {
	corpusCases := loadCorpus(t)

	matrix := map[formatCell][]string{}
	indexTotals := map[string]int{}

	for _, corpus := range corpusCases {
		for _, directory := range corpus.Directories {
			key := formatCell{directory.IndexFormat, directory.MappingFormat}
			matrix[key] = append(matrix[key], corpus.Name)
			indexTotals[directory.IndexFormat]++
		}
	}

	var keys []formatCell
	for key := range matrix {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].index != keys[j].index {
			return keys[i].index < keys[j].index
		}
		return keys[i].mapping < keys[j].mapping
	})

	t.Log("directory index format x data fork mapping format:")
	for _, key := range keys {
		t.Logf("  %-11s x %-8s  %4d directories  (%s)",
			key.index, key.mapping, len(matrix[key]), strings.Join(uniqueStrings(matrix[key]), ","))
	}

	for _, required := range []string{oracleIndexShortForm, oracleIndexBlock, oracleIndexLeaf, oracleIndexNode} {
		if indexTotals[required] == 0 {
			t.Errorf("corpus contains no directory in %s format; completeness across formats is unproven", required)
		}
	}
	if !hasMapping(matrix, "btree") {
		t.Errorf("corpus contains no directory whose data fork is b+tree mapped")
	}
	if !hasMapping(matrix, "extents") {
		t.Errorf("corpus contains no directory whose data fork is extent mapped")
	}
}

// formatCell is one cell of the index-format by mapping-format matrix.
type formatCell struct{ index, mapping string }

func hasMapping(matrix map[formatCell][]string, mapping string) bool {
	for key := range matrix {
		if key.mapping == mapping {
			return true
		}
	}
	return false
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	var unique []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			unique = append(unique, value)
		}
	}
	sort.Strings(unique)
	return unique
}

// TestWalkCompletenessAgainstOracle is the central test: every path the
// filesystem contains must appear in a libxfs walk, and nothing else may.
//
// Misses are attributed to the true on-disk format of the directory that
// should have produced them, which is the only way to tell a format-boundary
// defect from an unrelated one.
func TestWalkCompletenessAgainstOracle(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		t.Run(corpus.Name, func(t *testing.T) {
			volume := openCorpusVolume(t, corpus)
			result := walkVolume(t, volume)

			expected := corpus.expectedPaths()
			reportScanFailures(t, corpus, expected, result)
			missing := reportMissingPaths(t, corpus, expected, result)
			reportUnexpectedPaths(t, corpus, expected, result)
			reportAttributeMismatches(t, corpus, expected, result)
			reportPerDirectoryCounts(t, corpus, expected, result)

			source := "kernel mount"
			if !corpus.attributesKnown() {
				source = "xfs_db ncheck (image is not mountable; kinds and sizes unchecked)"
			}
			t.Logf("oracle %d paths via %s, libxfs %d paths, %d missing",
				len(expected)-1, source, len(result.Entries), missing)
		})
	}
}

// reportScanFailures turns a failed directory scan into the number of objects
// it cost, which is what makes a subtree loss visible instead of showing up as
// a single unexplained error.
func reportScanFailures(t *testing.T, corpus corpusCase, expected map[string]oracleRecord, result walkResult) {
	t.Helper()

	for _, failure := range result.Failures {
		lost := 0
		prefix := failure.Path
		if prefix != "/" {
			prefix += "/"
		}
		for path := range expected {
			if path != failure.Path && strings.HasPrefix(path, prefix) {
				lost++
			}
		}
		t.Errorf("directory %s (inode %d, on-disk format %s) failed to scan: %v -- %d oracle objects lie beneath it",
			failure.Path, failure.Ino, corpus.directoryFormatOf(failure.Ino), failure.Err, lost)
	}

	for _, directory := range result.Directories {
		if directory.Truncated {
			t.Errorf("directory %s (inode %d, on-disk format %s) reported a truncated scan",
				directory.Path, directory.Ino, corpus.directoryFormatOf(directory.Ino))
		}
	}
}

func reportMissingPaths(t *testing.T, corpus corpusCase, expected map[string]oracleRecord, result walkResult) int {
	t.Helper()

	byFormat := map[string][]string{}
	for path := range expected {
		if path == "/" {
			continue
		}
		if _, found := result.Entries[path]; found {
			continue
		}
		parent := parentPath(path)
		parentInode := corpus.NcheckPaths[parent]
		byFormat[corpus.directoryFormatOf(parentInode)] = append(byFormat[corpus.directoryFormatOf(parentInode)], path)
	}

	total := 0
	for _, format := range sortedKeys(byFormat) {
		paths := byFormat[format]
		total += len(paths)
		sort.Strings(paths)
		t.Errorf("%d paths missing from the walk, in directories of on-disk format %s (first: %s)",
			len(paths), format, strings.Join(firstN(paths, 5), ", "))
	}
	return total
}

func reportUnexpectedPaths(t *testing.T, corpus corpusCase, expected map[string]oracleRecord, result walkResult) {
	t.Helper()

	// When ground truth came from xfs_db alone it names each inode once, so a
	// hardlink alias is a path the oracle does not list even though the object
	// is real. Only a path whose inode is unknown entirely is a finding.
	knownInodes := map[uint64]bool{}
	if !corpus.attributesKnown() {
		for _, record := range expected {
			knownInodes[record.Ino] = true
		}
	}

	var extra []string
	for path, entry := range result.Entries {
		if _, found := expected[path]; found {
			continue
		}
		if !corpus.attributesKnown() && knownInodes[entry.Ino] {
			continue
		}
		extra = append(extra, path)
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		t.Errorf("%d paths reported by the walk that the filesystem does not contain (first: %s)",
			len(extra), strings.Join(firstN(extra, 5), ", "))
	}
}

func reportAttributeMismatches(t *testing.T, corpus corpusCase, expected map[string]oracleRecord, result walkResult) {
	t.Helper()

	var inodeMismatch, kindMismatch, sizeMismatch []string
	for path, found := range result.Entries {
		want, ok := expected[path]
		if !ok {
			continue
		}
		if found.Ino != want.Ino {
			inodeMismatch = append(inodeMismatch,
				fmt.Sprintf("%s: walk %d, oracle %d", path, found.Ino, want.Ino))
		}
		// Kind and size are only known when the kernel supplied them.
		if !corpus.attributesKnown() {
			continue
		}
		if found.Kind != want.Kind {
			kindMismatch = append(kindMismatch,
				fmt.Sprintf("%s: walk %s, oracle %s", path, found.Kind, want.Kind))
		}
		if found.Kind == want.Kind && found.Size != want.Size {
			sizeMismatch = append(sizeMismatch,
				fmt.Sprintf("%s: walk %d, oracle %d", path, found.Size, want.Size))
		}
	}

	for label, problems := range map[string][]string{
		"inode number": inodeMismatch,
		"object kind":  kindMismatch,
		"size":         sizeMismatch,
	} {
		if len(problems) > 0 {
			sort.Strings(problems)
			t.Errorf("%d %s mismatches (first: %s)", len(problems), label, strings.Join(firstN(problems, 5), "; "))
		}
	}
}

// reportPerDirectoryCounts compares directories one at a time.
//
// A whole-image total can be correct by accident, with one directory over
// reporting and another under reporting. Per-directory counts cannot.
func reportPerDirectoryCounts(t *testing.T, corpus corpusCase, expected map[string]oracleRecord, result walkResult) {
	t.Helper()

	wanted := map[string]int{}
	for path := range expected {
		if path == "/" {
			continue
		}
		wanted[parentPath(path)]++
	}

	found := map[string]int{}
	for _, entry := range result.Entries {
		found[parentPath(entry.Path)]++
	}

	for _, directory := range sortedKeys(wanted) {
		if wanted[directory] != found[directory] {
			inode := corpus.NcheckPaths[directory]
			t.Errorf("directory %s (inode %d, on-disk format %s): oracle has %d entries, walk found %d",
				directory, inode, corpus.directoryFormatOf(inode), wanted[directory], found[directory])
		}
	}
}

// TestDirectoryIndexAgreesWithDataWalk cross-checks each directory's hash
// index against its data blocks.
//
// This needs no oracle at all: the leaf index and the data blocks are two
// independent on-disk descriptions of the same entry set, so disagreement is
// proof of a defect in one of them regardless of what any external tool says.
func TestDirectoryIndexAgreesWithDataWalk(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		t.Run(corpus.Name, func(t *testing.T) {
			volume := openCorpusVolume(t, corpus)

			checked := 0
			for inode, directory := range corpus.Directories {
				if directory.IndexFormat == oracleIndexShortForm || directory.IndexFormat == oracleIndexBlock {
					continue
				}
				report, err := volume.VerifyDirectoryIndex(inode)
				if err != nil {
					t.Errorf("directory %s (inode %d, format %s): index verification failed: %v",
						directory.Path, inode, directory.IndexFormat, err)
					continue
				}
				checked++
				if !report.Consistent() {
					t.Errorf("directory %s (inode %d, format %s): hash index disagrees with the data blocks: "+
						"%d indexed, %d in data, %d missing from index, %d hash mismatches, %d dangling (missing: %s; mismatched: %s)",
						directory.Path, inode, directory.IndexFormat,
						report.IndexedEntries, report.DataEntries,
						len(report.MissingFromIndex), len(report.HashMismatches), report.DanglingIndexEntries,
						strings.Join(firstN(report.MissingFromIndex, 3), ","),
						strings.Join(firstN(report.HashMismatches, 3), ","))
				}
			}
			t.Logf("verified the hash index of %d indexed directories", checked)
		})
	}
}

// TestSourceFormatMatchesOracle checks that the directory format libxfs
// reports is the format the directory is actually in.
//
// The oracle derives it from xfs_db's block map and the inode's fork type;
// libxfs derives it from the extent list it parsed itself. Agreement means the
// two arrived at the same on-disk shape independently, which is what makes
// SourceFormat usable for attributing a defect to a format.
func TestSourceFormatMatchesOracle(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		t.Run(corpus.Name, func(t *testing.T) {
			volume := openCorpusVolume(t, corpus)

			counts := map[string]int{}
			for inodeNumber, directory := range corpus.Directories {
				listing, err := volume.ListDirectoryEntriesReport(inodeNumber)
				if err != nil {
					t.Errorf("directory %s (inode %d): %v", directory.Path, inodeNumber, err)
					continue
				}
				if listing.SourceFormat != directory.IndexFormat {
					t.Errorf("directory %s (inode %d): libxfs reports on-disk format %q, xfs_db shows %q",
						directory.Path, inodeNumber, listing.SourceFormat, directory.IndexFormat)
					continue
				}
				counts[listing.SourceFormat]++
			}
			t.Logf("formats agreed: %v", counts)
		})
	}
}

// hashLookupSampleLimit bounds how many names per directory are checked
// through the hash index. The corpus contains directories of 6000 entries and
// each lookup re-reads the index tree, so checking every name in every
// directory would dominate the run. Whatever is skipped is logged.
const hashLookupSampleLimit = 250

// TestHashLookupAgreesWithLinearWalk checks that resolving a name through the
// hash index gives the same answer as scanning the data blocks.
//
// The index is an optimisation, and an optimisation that disagrees with the
// data it indexes silently resolves paths to the wrong inode. This matters
// disproportionately because the index is consulted first, and its fallback
// only triggers when the index finds nothing -- never when it finds something
// wrong.
func TestHashLookupAgreesWithLinearWalk(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		t.Run(corpus.Name, func(t *testing.T) {
			volume := openCorpusVolume(t, corpus)

			checkedNames, checkedDirectories, skipped := 0, 0, 0
			for inodeNumber, directory := range corpus.Directories {
				inode, err := volume.OpenInode(inodeNumber)
				if err != nil {
					t.Errorf("directory %s (inode %d): %v", directory.Path, inodeNumber, err)
					continue
				}
				entries, err := volume.ListDirectoryEntries(inodeNumber)
				if err != nil {
					t.Errorf("directory %s (inode %d): %v", directory.Path, inodeNumber, err)
					continue
				}
				checkedDirectories++

				for i, entry := range entries {
					if i >= hashLookupSampleLimit {
						skipped += len(entries) - i
						break
					}
					checkedNames++
					found, ok := volume.lookupDirectoryEntryByHash(inode, entry.Name)
					if !ok {
						// Short-form and block directories have no index, so a
						// miss is expected and the linear walk is the only path.
						if directory.IndexFormat == oracleIndexLeaf || directory.IndexFormat == oracleIndexNode {
							t.Errorf("directory %s (format %s): %q exists but the hash index did not find it",
								directory.Path, directory.IndexFormat, entry.Name)
						}
						continue
					}
					if found.InodeNumber != entry.InodeNumber {
						t.Errorf("directory %s: %q resolves to inode %d through the hash index but %d through the data blocks",
							directory.Path, entry.Name, found.InodeNumber, entry.InodeNumber)
					}
				}
			}
			t.Logf("checked %d names across %d directories (%d names beyond the per-directory sample limit of %d were not checked)",
				checkedNames, checkedDirectories, skipped, hashLookupSampleLimit)
		})
	}
}

// TestFileContentMatchesOracle reads every regular file and compares its
// digest against the kernel's.
//
// Path completeness says nothing about whether the bytes are right. This is
// what catches an extent map that is plausible but wrong: holes in the wrong
// place, an unwritten range read as stale disk contents, or a b+tree-mapped
// file assembled out of order.
func TestFileContentMatchesOracle(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		t.Run(corpus.Name, func(t *testing.T) {
			if !corpus.attributesKnown() {
				t.Skipf("%s has no kernel oracle, so file digests are unknown", corpus.Name)
			}
			volume := openCorpusVolume(t, corpus)

			checked, bytesRead := 0, uint64(0)
			for path, want := range corpus.MountOracle {
				if want.Kind != "reg" || want.SHA256 == "" {
					continue
				}
				data, err := volume.ReadFileData(want.Ino)
				if err != nil {
					t.Errorf("%s (inode %d): %v", path, want.Ino, err)
					continue
				}
				if uint64(len(data)) != want.Size {
					t.Errorf("%s: read %d bytes, oracle reports size %d", path, len(data), want.Size)
					continue
				}
				if digest := fmt.Sprintf("%x", sha256.Sum256(data)); digest != want.SHA256 {
					t.Errorf("%s: content digest %s does not match the kernel's %s", path, digest, want.SHA256)
					continue
				}
				checked++
				bytesRead += want.Size
			}
			t.Logf("verified the contents of %d regular files (%d bytes)", checked, bytesRead)
		})
	}
}

// TestUnwrittenExtentsAreDistinguishedFromHoles checks that a preallocated
// range is reported as allocated-but-unwritten rather than as a hole.
//
// Both read back as zeros, so a reader cannot tell them apart, but they mean
// different things: a hole says nothing was ever stored there, while an
// unwritten extent names blocks that were reserved and whose previous contents
// may still be on the medium.
func TestUnwrittenExtentsAreDistinguishedFromHoles(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		if !corpus.attributesKnown() {
			continue
		}
		preallocated, ok := corpus.MountOracle["/files/unwritten.bin"]
		if !ok {
			continue
		}
		sparse, hasSparse := corpus.MountOracle["/files/sparse.bin"]
		if !hasSparse {
			continue
		}

		t.Run(corpus.Name, func(t *testing.T) {
			volume := openCorpusVolume(t, corpus)

			report, err := volume.InodeForensicReport(preallocated.Ino)
			if err != nil {
				t.Fatalf("preallocated file: %v", err)
			}
			unwritten, located := 0, 0
			for _, fragment := range report.Fragments {
				if !fragment.IsUnwritten {
					continue
				}
				unwritten++
				if !fragment.IsSparse {
					t.Errorf("unwritten fragment is not reported as reading back zeros: %+v", fragment)
				}
				if fragment.StartOffset != 0 {
					located++
				}
			}
			if unwritten == 0 {
				t.Errorf("a file created entirely by fallocate reported no unwritten extents")
			}
			if located == 0 {
				t.Errorf("unwritten extents were reported without a location, which is the only thing that makes them worth examining")
			}

			// The preallocated file must still read back as zeros.
			data, err := volume.ReadFileData(preallocated.Ino)
			if err != nil {
				t.Fatalf("reading the preallocated file: %v", err)
			}
			for i, b := range data {
				if b != 0 {
					t.Fatalf("preallocated byte %d read back as %#x, not zero", i, b)
				}
			}

			// A genuinely sparse file must have holes that are not unwritten.
			sparseReport, err := volume.InodeForensicReport(sparse.Ino)
			if err != nil {
				t.Fatalf("sparse file: %v", err)
			}
			holes := 0
			for _, fragment := range sparseReport.Fragments {
				if fragment.IsSparse && !fragment.IsUnwritten {
					holes++
				}
			}
			if holes == 0 {
				t.Errorf("a file written at scattered offsets reported no holes")
			}
			t.Logf("preallocated file: %d unwritten extents (%d located); sparse file: %d holes",
				unwritten, located, holes)
		})
	}
}

// deletedEntryRecallFloor is the number of deleted names the carver is
// required to recover from the corpus's `deleted` case.
//
// It is a pinned regression number, not a target. Recall is a property of what
// XFS happened to leave behind, so the honest thing to assert is that it has
// not got worse. The measured value at the time of writing is recorded in the
// test's output on every run.
//
// The corpus deletes two contiguous runs of 40 entries and 78 are recovered:
// each run loses exactly its first record, whose inode number is overwritten
// by the free-run header XFS writes in its place. The floor sits below that to
// tolerate allocator differences between xfsprogs versions, not to excuse a
// regression in the carver.
const deletedEntryRecallFloor = 75

// TestDeletedEntryRecovery checks what the directory carver claims about
// deleted entries.
//
// Two properties matter, and they are not equally negotiable. Soundness is
// absolute: a deleted name must never be reported as a live entry, and a live
// entry must never be missing, because a forensic report that confuses the two
// is worse than no report. Recall is not provable at all -- it depends on what
// the filesystem happened to leave behind -- so it is pinned as a floor and
// measured rather than asserted as a guarantee.
func TestDeletedEntryRecovery(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		if len(corpus.DeletedNames) == 0 {
			continue
		}
		t.Run(corpus.Name, func(t *testing.T) {
			volume := openCorpusVolume(t, corpus)

			directoryInode, ok := corpus.NcheckPaths["/target"]
			if !ok {
				t.Fatalf("the deleted case has no /target directory")
			}

			listing, err := volume.ScanDirectoryRecordsWithOptions(directoryInode,
				DirectoryScanOptions{BestEffort: true})
			if err != nil {
				t.Fatalf("scanning /target: %v", err)
			}

			deleted := map[string]bool{}
			for _, name := range corpus.DeletedNames {
				deleted[name] = true
			}
			surviving := map[string]bool{}
			for path := range corpus.expectedPaths() {
				if parentPath(path) == "/target" {
					surviving[path[strings.LastIndex(path, "/")+1:]] = true
				}
			}

			active := map[string]bool{}
			carved := map[string]bool{}
			var carvedNotDeleted []string
			for _, record := range listing.Records {
				switch record.Kind {
				case RecordKindActive:
					active[record.Name] = true
				case RecordKindCarved:
					// A candidate must never be presented as fact.
					if !record.IsProbabilistic() {
						t.Errorf("carved record %q is not marked probabilistic: %+v", record.Name, record)
					}
					if record.IsVerified() {
						t.Errorf("carved record %q claims to be verified: %+v", record.Name, record)
					}
					carved[record.Name] = true
					if !deleted[record.Name] && !surviving[record.Name] {
						carvedNotDeleted = append(carvedNotDeleted, record.Name)
					}
				}
			}

			// Soundness, in both directions.
			for name := range deleted {
				if active[name] {
					t.Errorf("deleted entry %q is reported as a live directory entry", name)
				}
			}
			for name := range surviving {
				if !active[name] {
					t.Errorf("surviving entry %q is missing from the active listing", name)
				}
			}
			for name := range active {
				if !surviving[name] {
					t.Errorf("entry %q is reported as live but the filesystem does not contain it", name)
				}
			}

			// Recall, measured and floored.
			recovered := 0
			for name := range deleted {
				if carved[name] {
					recovered++
				}
			}
			if recovered < deletedEntryRecallFloor {
				t.Errorf("recovered %d of %d deleted entries, below the pinned floor of %d",
					recovered, len(deleted), deletedEntryRecallFloor)
			}

			sort.Strings(carvedNotDeleted)
			t.Logf("recall: %d of %d deleted entries recovered (floor %d); "+
				"%d carved names matched nothing the directory ever held (first: %s)",
				recovered, len(deleted), deletedEntryRecallFloor,
				len(carvedNotDeleted), strings.Join(firstN(carvedNotDeleted, 5), ", "))
		})
	}
}

// TestDamagedBlockCostsOnlyThatBlock pins what a single unreadable directory
// block costs.
//
// A directory is a sequence of independently framed blocks. Damage to one says
// nothing about the others, so a walk that stops at the first bad block
// discards every later entry, and on a recursive walk that silently removes
// the whole subtree beneath it. The oracle describes the intact image; the
// walk runs against a copy with one block overwritten.
func TestDamagedBlockCostsOnlyThatBlock(t *testing.T) {
	for _, corpus := range loadCorpus(t) {
		if !corpus.hasDamagedImage() {
			continue
		}
		t.Run(corpus.Name, func(t *testing.T) {
			volume, err := OpenVolumeFromPath(corpus.DamagedImagePath)
			if err != nil {
				t.Fatalf("opening %s: %v", corpus.DamagedImagePath, err)
			}
			t.Cleanup(func() { _ = volume.Close() })

			intact, err := parseNcheckDirectoryEntryCount(corpus, corpus.DamagedDirectory)
			if err != nil {
				t.Fatalf("counting intact entries: %v", err)
			}

			listing, scanErr := volume.ListDirectoryEntriesReport(corpus.DamagedDirectory)

			// The loss must be visible. A partial listing returned as though
			// it were complete is the worst possible outcome.
			if scanErr == nil {
				t.Errorf("scanning a directory with a destroyed block returned no error; the loss is invisible to callers")
			}
			if len(listing.Anomalies) == 0 {
				t.Errorf("scanning a directory with a destroyed block recorded no anomaly")
			}

			// The recovered entries must come from more than one block, which
			// is only possible if the walk continued past the damaged one.
			if len(listing.Entries) == 0 {
				t.Fatalf("one destroyed block cost the entire directory: %d of %d entries recovered",
					len(listing.Entries), intact)
			}
			if len(listing.Entries) >= intact {
				t.Errorf("expected some entries to be lost with the destroyed block, got %d of %d",
					len(listing.Entries), intact)
			}

			perBlock := entriesPerDirectoryBlock(corpus)
			if recovered, floor := len(listing.Entries), intact-perBlock; recovered < floor {
				t.Errorf("one destroyed block cost more than itself: %d of %d entries recovered, "+
					"but only the %d entries in that block should have been lost",
					recovered, intact, perBlock)
			}

			// The decisive check. The corpus creates this directory's entries in
			// ascending name order, and XFS fills data blocks in creation
			// order, so the greatest name lives in the last data block. If it
			// is present, the walk demonstrably continued past the damaged
			// block rather than stopping there; a count-based floor alone
			// cannot distinguish those two outcomes.
			last := greatestChildName(corpus, corpus.DamagedDirectory)
			if last == "" {
				t.Fatalf("could not determine the last-created entry of the damaged directory")
			}
			found := false
			for _, entry := range listing.Entries {
				if entry.Name == last {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("entry %q lives in a data block after the damaged one but was not recovered: "+
					"the walk stopped at the damaged block instead of skipping it", last)
			}

			// Best effort must not recover less than the strict path.
			best, _ := volume.ScanDirectoryRecordsWithOptions(corpus.DamagedDirectory,
				DirectoryScanOptions{BestEffort: true})
			if len(best.Entries) < len(listing.Entries) {
				t.Errorf("best-effort recovered %d entries, fewer than the strict scan's %d",
					len(best.Entries), len(listing.Entries))
			}

			// The rest of the filesystem must still be reachable: a damaged
			// directory is not a damaged volume.
			if _, err := volume.ResolveInodeByPath("/intact/keep.txt"); err != nil {
				t.Errorf("a sibling directory became unreachable because another directory was damaged: %v", err)
			}

			t.Logf("recovered %d of %d entries after destroying one directory block (~%d entries per block); scan reported: %v",
				len(listing.Entries), intact, perBlock, scanErr)
		})
	}
}

// parseNcheckDirectoryEntryCount counts the oracle's direct children of a
// directory, identified by inode number.
func parseNcheckDirectoryEntryCount(corpus corpusCase, directoryInode uint64) (int, error) {
	directory, ok := corpus.Directories[directoryInode]
	if !ok {
		return 0, fmt.Errorf("inode %d is not a directory in the oracle", directoryInode)
	}
	count := 0
	for path := range corpus.expectedPaths() {
		if path != directory.Path && parentPath(path) == directory.Path {
			count++
		}
	}
	return count, nil
}

// greatestChildName returns the lexicographically greatest direct child name
// of a directory according to the oracle.
func greatestChildName(corpus corpusCase, directoryInode uint64) string {
	directory, ok := corpus.Directories[directoryInode]
	if !ok {
		return ""
	}
	greatest := ""
	for path := range corpus.expectedPaths() {
		if path == directory.Path || parentPath(path) != directory.Path {
			continue
		}
		if name := path[strings.LastIndex(path, "/")+1:]; name > greatest {
			greatest = name
		}
	}
	return greatest
}

// entriesPerDirectoryBlock bounds how many entries one directory block can
// hold, used as the ceiling on what a single destroyed block may cost.
//
// The corpus names are short and fixed, so the bound is the block payload
// divided by the smallest possible entry: inumber(8) + namelen(1) + ftype(1) +
// tag(2) plus the name, rounded up to eight bytes. Sixteen bytes is the
// smallest such entry, which makes this a genuine upper bound.
func entriesPerDirectoryBlock(corpus corpusCase) int {
	const smallestEntry = 16
	if corpus.DirectoryBlockSize == 0 {
		return 0
	}
	return int(corpus.DirectoryBlockSize / smallestEntry)
}

func parentPath(path string) string {
	index := strings.LastIndex(path, "/")
	if index <= 0 {
		return "/"
	}
	return path[:index]
}

func sortedKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func firstN(values []string, n int) []string {
	if len(values) <= n {
		return values
	}
	return append(append([]string{}, values[:n]...), "...")
}
