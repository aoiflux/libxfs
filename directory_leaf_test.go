package libxfs

import (
	"sort"
	"testing"
)

// leafIndexFor builds the hash index entries matching a set of directory
// entries laid out in a single data block, sorted by hash as XFS keeps them.
func leafIndexFor(image *fixtureImage, entries []fixtureEntry) []fixtureLeafEntry {
	index := make([]fixtureLeafEntry, 0, len(entries))

	offset := fixtureDir3DataHeaderSize
	if image.formatVersion == 4 {
		offset = fixtureDir2DataHeaderSize
	}
	for _, entry := range entries {
		if entry.name != "." && entry.name != ".." {
			index = append(index, fixtureLeafEntry{
				hash:    fixtureHashName(entry.name),
				address: uint32(offset / 8),
			})
		}
		offset += fixtureEntryLength(len(entry.name), image.hasFileType())
	}

	sort.Slice(index, func(i, j int) bool { return index[i].hash < index[j].hash })
	return index
}

// TestDirectoryHashMatchesFormat pins the hash to values computed by an
// independent implementation of xfs_da_hashname.
func TestDirectoryHashMatchesFormat(t *testing.T) {
	for _, name := range []string{
		"a", "ab", "abc", "abcd", "abcde",
		"file.txt", "a-very-long-directory-entry-name-for-hashing",
		"", "..",
	} {
		if got, want := xfsDirHashName([]byte(name)), fixtureHashName(name); got != want {
			t.Fatalf("hash(%q) = 0x%08x, want 0x%08x", name, got, want)
		}
	}
}

// TestDirectoryLeafIndexParsed covers 3.1: reading the hash index that lives
// above the 32 GiB data-space boundary.
func TestDirectoryLeafIndexParsed(t *testing.T) {
	for _, formatVersion := range []uint8{4, 5} {
		image := newFixtureImage(formatVersion, 0)

		entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
			namedEntries("a", 8, 100)...)
		dataBlock := image.buildDirectoryBlock(image.directoryDataMagic(), entries)
		index := leafIndexFor(image, entries)
		leafBlock := image.buildDirectoryLeafBlock(index, false)

		image.addLeafDirectory(fixtureFirstInode, [][]byte{dataBlock}, [][]byte{leafBlock})
		linkRoot(image, fixtureFirstInode)

		volume := image.open(t)
		inode, err := volume.OpenInode(fixtureFirstInode)
		if err != nil {
			t.Fatalf("v%d: OpenInode failed: %v", formatVersion, err)
		}

		got, err := volume.collectDirectoryLeafEntries(inode)
		if err != nil {
			t.Fatalf("v%d: collecting leaf entries failed: %v", formatVersion, err)
		}
		if len(got) != len(index) {
			t.Fatalf("v%d: expected %d index entries, got %d", formatVersion, len(index), len(got))
		}
		for i := range index {
			if got[i].Hash != index[i].hash || got[i].Address != index[i].address {
				t.Fatalf("v%d: index entry %d mismatch: got %+v want %+v",
					formatVersion, i, got[i], index[i])
			}
		}

		// Listing must be unaffected: entries live in the data blocks.
		listed, err := volume.ListDirectoryEntries(fixtureFirstInode)
		if err != nil {
			t.Fatalf("v%d: listing failed: %v", formatVersion, err)
		}
		if len(listed) != 8 {
			t.Fatalf("v%d: expected 8 entries, got %d", formatVersion, len(listed))
		}
	}
}

// TestDirectoryNodeIndexParsed covers the node-format case, where the leaf
// space holds a da node tree rather than a single leaf block.
func TestDirectoryNodeIndexParsed(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		namedEntries("a", 10, 100)...)
	dataBlock := image.buildDirectoryBlock(image.directoryDataMagic(), entries)

	index := leafIndexFor(image, entries)
	half := len(index) / 2
	leafOne := image.buildDirectoryLeafBlock(index[:half], true)
	leafTwo := image.buildDirectoryLeafBlock(index[half:], true)

	// The root node is placed first in the leaf space, with the two leaves
	// after it; the node's children reference their logical block numbers.
	placeholder := make([]byte, image.dirBlockSize)
	logical := image.addLeafDirectory(fixtureFirstInode,
		[][]byte{dataBlock},
		[][]byte{placeholder, leafOne, leafTwo})

	root := image.buildDirectoryNodeBlock([]uint32{logical[1], logical[2]}, 1)
	// Overwrite the placeholder now that the child block numbers are known.
	image.writeAt(uint64(image.leafBlockDiskOffset(0)), root)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	inode, err := volume.OpenInode(fixtureFirstInode)
	if err != nil {
		t.Fatalf("OpenInode failed: %v", err)
	}

	got, err := volume.collectDirectoryLeafEntries(inode)
	if err != nil {
		t.Fatalf("collecting node-tree leaf entries failed: %v", err)
	}
	if len(got) != len(index) {
		t.Fatalf("expected %d index entries across the node tree, got %d", len(index), len(got))
	}
}

// TestDirectoryHashLookupResolvesPath covers 3.2: resolution through the hash
// index rather than a linear walk.
func TestDirectoryHashLookupResolvesPath(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		namedEntries("a", 12, 100)...)
	dataBlock := image.buildDirectoryBlock(image.directoryDataMagic(), entries)
	leafBlock := image.buildDirectoryLeafBlock(leafIndexFor(image, entries), false)

	image.addLeafDirectory(fixtureFirstInode, [][]byte{dataBlock}, [][]byte{leafBlock})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	inode, err := volume.OpenInode(fixtureFirstInode)
	if err != nil {
		t.Fatalf("OpenInode failed: %v", err)
	}

	entry, ok := volume.lookupDirectoryEntryByHash(inode, "a7")
	if !ok {
		t.Fatal("hash lookup did not find an entry that exists")
	}
	if entry.InodeNumber != 107 {
		t.Fatalf("hash lookup returned inode %d, want 107", entry.InodeNumber)
	}

	// Path resolution must agree with the linear walk.
	resolved, err := volume.ResolveInodeByPath("/dir/a7")
	if err != nil {
		t.Fatalf("ResolveInodeByPath failed: %v", err)
	}
	if resolved != 107 {
		t.Fatalf("resolved inode %d, want 107", resolved)
	}
}

// TestDirectoryHashLookupFallsBackWhenIndexIsBroken is the safety property for
// 3.2: an unusable index must never make a resolvable name unresolvable.
func TestDirectoryHashLookupFallsBackWhenIndexIsBroken(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		namedEntries("a", 12, 100)...)
	dataBlock := image.buildDirectoryBlock(image.directoryDataMagic(), entries)

	// An index whose addresses point nowhere useful.
	broken := leafIndexFor(image, entries)
	for i := range broken {
		broken[i].address = 0xffffff
	}
	leafBlock := image.buildDirectoryLeafBlock(broken, false)

	image.addLeafDirectory(fixtureFirstInode, [][]byte{dataBlock}, [][]byte{leafBlock})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)

	resolved, err := volume.ResolveInodeByPath("/dir/a7")
	if err != nil {
		t.Fatalf("resolution must fall back to the linear walk, got: %v", err)
	}
	if resolved != 107 {
		t.Fatalf("resolved inode %d, want 107", resolved)
	}
}

// TestVerifyDirectoryIndexDetectsDivergence covers 3.3: the forensic signal.
// The kernel maintains the index and the data blocks together, so disagreement
// means something modified the directory without maintaining both.
func TestVerifyDirectoryIndexDetectsDivergence(t *testing.T) {
	buildImage := func(mutate func(index []fixtureLeafEntry) []fixtureLeafEntry) *Volume {
		image := newFixtureImage(5, 0)
		entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
			namedEntries("a", 10, 100)...)
		dataBlock := image.buildDirectoryBlock(image.directoryDataMagic(), entries)
		index := leafIndexFor(image, entries)
		if mutate != nil {
			index = mutate(index)
		}
		leafBlock := image.buildDirectoryLeafBlock(index, false)
		image.addLeafDirectory(fixtureFirstInode, [][]byte{dataBlock}, [][]byte{leafBlock})
		linkRoot(image, fixtureFirstInode)
		return image.open(t)
	}

	t.Run("consistent", func(t *testing.T) {
		volume := buildImage(nil)
		report, err := volume.VerifyDirectoryIndex(fixtureFirstInode)
		if err != nil {
			t.Fatalf("VerifyDirectoryIndex failed: %v", err)
		}
		if !report.HasIndex {
			t.Fatal("expected the directory to report a hash index")
		}
		if !report.Consistent() {
			t.Fatalf("a healthy directory must verify clean: %+v", report)
		}
		if report.DataEntries != 10 || report.IndexedEntries != 10 {
			t.Fatalf("unexpected counts: %+v", report)
		}
	})

	t.Run("entry missing from index", func(t *testing.T) {
		volume := buildImage(func(index []fixtureLeafEntry) []fixtureLeafEntry {
			return index[1:] // drop one indexed entry
		})
		report, err := volume.VerifyDirectoryIndex(fixtureFirstInode)
		if err != nil {
			t.Fatalf("VerifyDirectoryIndex failed: %v", err)
		}
		if report.Consistent() {
			t.Fatalf("expected divergence to be reported: %+v", report)
		}
		if len(report.MissingFromIndex) != 1 {
			t.Fatalf("expected one entry missing from the index: %+v", report)
		}
		if !hasAnomaly(report.Anomalies, "directory_index_divergence") {
			t.Fatalf("expected a divergence anomaly: %+v", report.Anomalies)
		}
	})

	t.Run("index points at the wrong entry", func(t *testing.T) {
		volume := buildImage(func(index []fixtureLeafEntry) []fixtureLeafEntry {
			mutated := append([]fixtureLeafEntry(nil), index...)
			mutated[0].address += 2 // same hash, different location
			return mutated
		})
		report, err := volume.VerifyDirectoryIndex(fixtureFirstInode)
		if err != nil {
			t.Fatalf("VerifyDirectoryIndex failed: %v", err)
		}
		if report.Consistent() {
			t.Fatalf("expected divergence to be reported: %+v", report)
		}
	})
}

// TestVerifyDirectoryIndexWithoutIndex checks that directories with no hash
// index are reported as such rather than as inconsistent.
func TestVerifyDirectoryIndexWithoutIndex(t *testing.T) {
	image := newFixtureImage(5, 0)
	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile})
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	report, err := volume.VerifyDirectoryIndex(fixtureFirstInode)
	if err != nil {
		t.Fatalf("VerifyDirectoryIndex failed: %v", err)
	}
	if report.HasIndex {
		t.Fatal("a single-block directory has no hash index")
	}
	if !report.Consistent() {
		t.Fatalf("a directory without an index cannot diverge: %+v", report)
	}
}
