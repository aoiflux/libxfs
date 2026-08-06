package libxfs

import (
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

// linkRoot points the root directory at a single child directory inode so
// that path based APIs can reach the fixture directory.
func linkRoot(image *fixtureImage, childInode uint64) {
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, []fixtureEntry{
		{name: "dir", inodeNumber: childInode, fileType: DirEntryFileTypeDirectory},
	})
}

func assertNames(t *testing.T, got []DirectoryEntry, want ...string) {
	t.Helper()
	names := entryNames(got)
	if len(names) != len(want) {
		t.Fatalf("entry count mismatch: got %v want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("entry %d mismatch: got %v want %v", i, names, want)
		}
	}
}

// TestListDirectoryEntriesV5BlockDirectory is the regression test for the v5
// directory data header size.
//
// xfs_dir3_data_hdr is 64 bytes: xfs_dir3_blk_hdr(48) + best_free[3](12) +
// pad(4). Parsing a v5 block directory with a 56 byte header starts eight
// bytes early and misreads the first entry.
func TestListDirectoryEntriesV5BlockDirectory(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile},
		fixtureEntry{name: "beta", inodeNumber: 41, fileType: DirEntryFileTypeDirectory},
	)
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	got, err := volume.ListDirectoryEntries(fixtureFirstInode)
	if err != nil {
		t.Fatalf("ListDirectoryEntries failed: %v", err)
	}
	assertNames(t, got, "alpha", "beta")

	if got[0].InodeNumber != 40 || got[1].InodeNumber != 41 {
		t.Fatalf("inode numbers mismatch: %+v", got)
	}
	if got[0].FileType != DirEntryFileTypeRegularFile || got[1].FileType != DirEntryFileTypeDirectory {
		t.Fatalf("file types mismatch: %+v", got)
	}
}

func TestListDirectoryEntriesV4BlockDirectory(t *testing.T) {
	image := newFixtureImage(4, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40},
		fixtureEntry{name: "beta", inodeNumber: 41},
	)
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	got, err := volume.ListDirectoryEntries(fixtureFirstInode)
	if err != nil {
		t.Fatalf("ListDirectoryEntries failed: %v", err)
	}
	assertNames(t, got, "alpha", "beta")
}

// TestListDirectoryEntriesMultiBlockDirectory covers leaf/node format
// directories, where entries span several data blocks.
func TestListDirectoryEntriesMultiBlockDirectory(t *testing.T) {
	for _, formatVersion := range []uint8{4, 5} {
		image := newFixtureImage(formatVersion, 0)

		first := append(dotEntries(fixtureFirstInode, fixtureRootInode),
			namedEntries("a", 20, 100)...)
		blocks := [][]byte{
			image.buildDirectoryBlock(image.directoryDataMagic(), first),
			image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 20, 200)),
			image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("c", 20, 300)),
		}
		image.addBlockDirectory(fixtureFirstInode, blocks)
		linkRoot(image, fixtureFirstInode)

		volume := image.open(t)
		listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{})
		if err != nil {
			t.Fatalf("v%d: listing failed: %v", formatVersion, err)
		}

		if len(listing.Entries) != 60 {
			t.Fatalf("v%d: expected 60 entries across 3 blocks, got %d", formatVersion, len(listing.Entries))
		}
		if listing.Format != DirectoryFormatMultiBlock {
			t.Fatalf("v%d: expected multi-block format, got %q", formatVersion, listing.Format)
		}
		if listing.BlocksScanned != 3 {
			t.Fatalf("v%d: expected 3 blocks scanned, got %d", formatVersion, listing.BlocksScanned)
		}
		if listing.Entries[0].Name != "a0" || listing.Entries[59].Name != "c19" {
			t.Fatalf("v%d: unexpected ordering: first=%s last=%s",
				formatVersion, listing.Entries[0].Name, listing.Entries[59].Name)
		}
	}
}

// TestListDirectoryEntriesLargeDirectoryBlock covers a directory block size
// larger than the filesystem block size (sb_dirblklog > 0).
func TestListDirectoryEntriesLargeDirectoryBlock(t *testing.T) {
	image := newFixtureImage(5, 4) // 64 KiB directory blocks on 4 KiB fs blocks
	if image.dirBlockSize != 65536 {
		t.Fatalf("unexpected directory block size: %d", image.dirBlockSize)
	}

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("x", 100, 100)...)
	blocks := [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), first),
		image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("y", 100, 500)),
	}
	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{})
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(listing.Entries) != 200 {
		t.Fatalf("expected 200 entries, got %d", len(listing.Entries))
	}
	if listing.BlocksScanned != 2 {
		t.Fatalf("expected 2 directory blocks scanned, got %d", listing.BlocksScanned)
	}
}

// TestListDirectoryEntriesSkipsHoles covers freed data blocks, which XFS
// punches out of the directory's logical space.
func TestListDirectoryEntriesSkipsHoles(t *testing.T) {
	image := newFixtureImage(5, 0)

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 5, 100)...)
	blocks := [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), first),
		make([]byte, image.dirBlockSize), // hole
		image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("c", 5, 300)),
	}
	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{})
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	if len(listing.Entries) != 10 {
		t.Fatalf("expected 10 entries across the hole, got %d", len(listing.Entries))
	}
	if len(listing.Anomalies) != 0 {
		t.Fatalf("a hole is normal and must not raise anomalies: %+v", listing.Anomalies)
	}
}

// TestListDirectoryEntriesBtreeDirectory covers a directory whose data fork is
// an extent b-tree, exercising the b-tree extent mapping and the directory
// walker together.
func TestListDirectoryEntriesBtreeDirectory(t *testing.T) {
	for _, formatVersion := range []uint8{4, 5} {
		image := newFixtureImage(formatVersion, 0)

		first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 15, 100)...)
		blocks := [][]byte{
			image.buildDirectoryBlock(image.directoryDataMagic(), first),
			image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 15, 200)),
		}
		image.addBtreeDirectory(fixtureFirstInode, blocks)
		linkRoot(image, fixtureFirstInode)

		volume := image.open(t)

		inode, err := volume.OpenInode(fixtureFirstInode)
		if err != nil {
			t.Fatalf("v%d: OpenInode failed: %v", formatVersion, err)
		}
		if inode.ForkType != ForkTypeBtree {
			t.Fatalf("v%d: expected a btree fork, got %d", formatVersion, inode.ForkType)
		}

		listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{})
		if err != nil {
			t.Fatalf("v%d: btree directory listing failed: %v", formatVersion, err)
		}
		if len(listing.Entries) != 30 {
			t.Fatalf("v%d: expected 30 entries, got %d", formatVersion, len(listing.Entries))
		}
		if listing.Entries[0].Name != "a0" || listing.Entries[29].Name != "b14" {
			t.Fatalf("v%d: unexpected ordering: %v", formatVersion, entryNames(listing.Entries))
		}
	}
}

func TestResolveInodeByPathThroughMultiBlockDirectory(t *testing.T) {
	image := newFixtureImage(5, 0)

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 30, 100)...)
	blocks := [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), first),
		image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 30, 200)),
	}
	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)

	// The target lives in the second data block, which is only reachable once
	// the walker iterates past block zero.
	got, err := volume.ResolveInodeByPath("/dir/b17")
	if err != nil {
		t.Fatalf("ResolveInodeByPath failed: %v", err)
	}
	if got != 217 {
		t.Fatalf("unexpected inode: got %d want 217", got)
	}

	if _, err := volume.ResolveInodeByPath("/dir/missing"); !errors.Is(err, ErrInodeNotFound) {
		t.Fatalf("expected ErrInodeNotFound, got %v", err)
	}
}

func TestDirectoryParentInode(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile})
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)

	parent, err := volume.DirectoryParentInode(fixtureFirstInode)
	if err != nil {
		t.Fatalf("DirectoryParentInode failed: %v", err)
	}
	if parent != fixtureRootInode {
		t.Fatalf("block directory parent mismatch: got %d want %d", parent, fixtureRootInode)
	}

	rootParent, err := volume.DirectoryParentInode(fixtureRootInode)
	if err != nil {
		t.Fatalf("DirectoryParentInode on short-form failed: %v", err)
	}
	if rootParent != fixtureRootInode {
		t.Fatalf("short-form parent mismatch: got %d want %d", rootParent, fixtureRootInode)
	}
}

// TestScanDirectoryRecordsCarvesAcrossBlocks checks that carved candidates are
// addressable: within a multi-block directory, a within-block offset alone is
// ambiguous.
func TestScanDirectoryRecordsCarvesAcrossBlocks(t *testing.T) {
	image := newFixtureImage(5, 0)

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 3, 100)...)
	blockZero := image.buildDirectoryBlock(image.directoryDataMagic(), first)

	// Second block: a free run covering everything after the header, holding
	// a stale entry that survived deletion.
	blockOne := make([]byte, image.dirBlockSize)
	copy(blockOne[0:4], image.directoryDataMagic())
	image.markFreeRun(blockOne, fixtureDir3DataHeaderSize, len(blockOne))

	ghostOffset := fixtureDir3DataHeaderSize + 32
	writeRawDirectoryEntry(blockOne, ghostOffset, fixtureEntry{
		name:        "deleted-file",
		inodeNumber: 999,
		fileType:    DirEntryFileTypeRegularFile,
	}, true)

	image.addBlockDirectory(fixtureFirstInode, [][]byte{blockZero, blockOne})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	listing, err := volume.ScanDirectoryRecordsWithOptions(fixtureFirstInode, DirectoryScanOptions{})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	var carved *DirectoryRecord
	for i := range listing.Records {
		if listing.Records[i].Name == "deleted-file" {
			carved = &listing.Records[i]
		}
	}
	if carved == nil {
		t.Fatalf("carved candidate not recovered: %+v", listing.Records)
	}

	if carved.Kind != RecordKindCarved || !carved.IsCarved || !carved.IsDeleted {
		t.Fatalf("carved record mislabelled: %+v", carved)
	}
	if carved.IsVerified() || !carved.IsProbabilistic() {
		t.Fatalf("carved record must not read as verified: %+v", carved)
	}
	if carved.BlockIndex != 1 {
		t.Fatalf("carved record block index mismatch: got %d want 1", carved.BlockIndex)
	}
	wantOffset := uint64(image.dirBlockSize) + uint64(ghostOffset)
	if carved.LogicalOffset != wantOffset {
		t.Fatalf("carved logical offset mismatch: got %d want %d", carved.LogicalOffset, wantOffset)
	}
	if carved.Confidence != ConfidenceHigh {
		t.Fatalf("expected high confidence from a matching tag, got %q", carved.Confidence)
	}
	if !hasReason(carved.ConfidenceReasons, ReasonTagMatchesOffset) {
		t.Fatalf("expected tag evidence, got %v", carved.ConfidenceReasons)
	}

	// Active records from block zero must remain distinguishable.
	for _, record := range listing.Records {
		if record.Name == "a0" {
			if !record.IsVerified() || record.IsProbabilistic() {
				t.Fatalf("active record mislabelled: %+v", record)
			}
			if record.Kind != RecordKindActive {
				t.Fatalf("active record kind mismatch: %+v", record)
			}
		}
	}
}

func TestScanDirectoryRecordsFreeSlotIsNotAnEntry(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile})
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	records, err := volume.ScanDirectoryRecords(fixtureFirstInode)
	if err != nil {
		t.Fatalf("ScanDirectoryRecords failed: %v", err)
	}

	freeSlots := 0
	for _, record := range records {
		if record.Kind != RecordKindFreeSlot {
			continue
		}
		freeSlots++
		if record.Name != "" || record.InodeNumber != 0 {
			t.Fatalf("free slot must not carry a recovered identity: %+v", record)
		}
		if record.IsVerified() {
			t.Fatalf("free slot must not read as verified: %+v", record)
		}
		if record.Confidence != ConfidenceLow {
			t.Fatalf("free slot confidence mismatch: %+v", record)
		}
	}
	if freeSlots != 1 {
		t.Fatalf("expected exactly one free slot record, got %d: %+v", freeSlots, records)
	}
}

// TestBestEffortRecoversPartialDirectory checks that a damaged block does not
// discard entries already recovered from healthy blocks.
func TestBestEffortRecoversPartialDirectory(t *testing.T) {
	image := newFixtureImage(5, 0)

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 10, 100)...)
	blockZero := image.buildDirectoryBlock(image.directoryDataMagic(), first)

	blockOne := image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 4, 200))
	// Damage the free-run header that covers the rest of the block, so its
	// declared length runs past the end of the entry region.
	freeRunOffset := fixtureDir3DataHeaderSize + 4*fixtureEntryLength(len("b0"), true)
	binary.BigEndian.PutUint16(blockOne[freeRunOffset+2:freeRunOffset+4], 0xfffe)

	image.addBlockDirectory(fixtureFirstInode, [][]byte{blockZero, blockOne})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)

	if _, err := volume.ListDirectoryEntries(fixtureFirstInode); err == nil {
		t.Fatal("strict mode must reject a malformed directory entry")
	}

	listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{BestEffort: true})
	if err != nil {
		t.Fatalf("best-effort listing failed: %v", err)
	}
	if len(listing.Entries) < 11 {
		t.Fatalf("best-effort must retain entries recovered before the damage, got %d", len(listing.Entries))
	}
	if len(listing.Anomalies) == 0 {
		t.Fatal("best-effort recovery must report an anomaly")
	}
	if listing.Entries[0].Name != "a0" {
		t.Fatalf("unexpected first entry: %+v", listing.Entries[0])
	}
}

// TestDirectorySizeIsBounded checks that an implausible di_size neither
// allocates unbounded memory nor drives an unbounded walk.
func TestDirectorySizeIsBounded(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile})
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	// A directory claiming to be larger than the entire volume.
	image.setInodeSize(fixtureFirstInode, 1<<62)

	volume := image.open(t)
	listing, err := volume.ScanDirectoryRecordsWithOptions(fixtureFirstInode, DirectoryScanOptions{
		BestEffort: true,
		MaxBlocks:  64,
	})
	if err != nil {
		t.Fatalf("bounded scan failed: %v", err)
	}
	if listing.BlocksScanned > 64 {
		t.Fatalf("scan exceeded the block cap: %d", listing.BlocksScanned)
	}
	if !hasAnomaly(listing.Anomalies, "directory_size_exceeds_volume") {
		t.Fatalf("expected a volume-capacity anomaly, got %+v", listing.Anomalies)
	}
}

func TestDirectoryUnalignedSizeIsReported(t *testing.T) {
	image := newFixtureImage(5, 0)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile})
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	image.setInodeSize(fixtureFirstInode, uint64(image.dirBlockSize)-9)

	volume := image.open(t)
	listing, err := volume.ScanDirectoryRecordsWithOptions(fixtureFirstInode, DirectoryScanOptions{BestEffort: true})
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if !hasAnomaly(listing.Anomalies, "directory_size_unaligned") {
		t.Fatalf("expected an alignment anomaly, got %+v", listing.Anomalies)
	}
}

func TestDirectoryEntryCapTruncates(t *testing.T) {
	image := newFixtureImage(5, 0)

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 30, 100)...)
	blocks := [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), first),
		image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 30, 200)),
	}
	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{MaxEntries: 10})
	if err != nil {
		t.Fatalf("capped listing failed: %v", err)
	}
	if len(listing.Records) != 10 {
		t.Fatalf("expected the cap to apply, got %d records", len(listing.Records))
	}
	if !listing.Truncated {
		t.Fatal("a capped listing must report truncation rather than look complete")
	}
}

// TestLargeDirectoryListing exercises a wide directory spanning many blocks,
// with long names, and asserts completeness and order.
func TestLargeDirectoryListing(t *testing.T) {
	image := newFixtureImage(5, 0)

	const entriesPerBlock = 60
	const blockCount = 20

	blocks := make([][]byte, 0, blockCount)
	expected := make([]string, 0, entriesPerBlock*blockCount)
	inode := uint64(1000)

	for b := 0; b < blockCount; b++ {
		entries := make([]fixtureEntry, 0, entriesPerBlock+2)
		if b == 0 {
			entries = append(entries, dotEntries(fixtureFirstInode, fixtureRootInode)...)
		}
		for i := 0; i < entriesPerBlock; i++ {
			name := "block" + itoa(b) + "_entry" + itoa(i)
			entries = append(entries, fixtureEntry{
				name:        name,
				inodeNumber: inode,
				fileType:    DirEntryFileTypeRegularFile,
			})
			expected = append(expected, name)
			inode++
		}
		blocks = append(blocks, image.buildDirectoryBlock(image.directoryDataMagic(), entries))
	}

	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	got, err := volume.ListDirectoryEntries(fixtureFirstInode)
	if err != nil {
		t.Fatalf("large directory listing failed: %v", err)
	}
	if len(got) != len(expected) {
		t.Fatalf("expected %d entries, got %d", len(expected), len(got))
	}
	for i := range expected {
		if got[i].Name != expected[i] {
			t.Fatalf("entry %d mismatch: got %q want %q", i, got[i].Name, expected[i])
		}
	}
}

func TestLongDirectoryEntryName(t *testing.T) {
	image := newFixtureImage(5, 0)

	longName := strings.Repeat("n", maxDirectoryNameBytes)
	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: longName, inodeNumber: 40, fileType: DirEntryFileTypeRegularFile},
		fixtureEntry{name: "after", inodeNumber: 41, fileType: DirEntryFileTypeRegularFile},
	)
	block := image.buildDirectoryBlock(image.directoryDataMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	got, err := volume.ListDirectoryEntries(fixtureFirstInode)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	assertNames(t, got, longName, "after")
}

// TestUnexpectedBlockInDataSpace covers a leaf block appearing below di_size,
// which should never happen on a healthy filesystem.
func TestUnexpectedBlockInDataSpace(t *testing.T) {
	image := newFixtureImage(5, 0)

	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 4, 100)...)

	leafBlock := make([]byte, image.dirBlockSize)
	binary.BigEndian.PutUint16(leafBlock[daBlockInfoMagicOffset:daBlockInfoMagicOffset+2], dirLeaf1MagicV5)

	image.addBlockDirectory(fixtureFirstInode, [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), first),
		leafBlock,
	})
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)

	if _, err := volume.ListDirectoryEntries(fixtureFirstInode); !errors.Is(err, ErrUnsupportedDirFormat) {
		t.Fatalf("strict mode should reject a leaf block in the data space, got %v", err)
	}

	listing, err := volume.ListDirectoryEntriesWithOptions(fixtureFirstInode, DirectoryScanOptions{BestEffort: true})
	if err != nil {
		t.Fatalf("best-effort listing failed: %v", err)
	}
	if len(listing.Entries) != 4 {
		t.Fatalf("expected the healthy block's entries, got %d", len(listing.Entries))
	}
	if !hasAnomaly(listing.Anomalies, "directory_unexpected_block") {
		t.Fatalf("expected an unexpected-block anomaly, got %+v", listing.Anomalies)
	}
}

func TestShortFormDirectoryExposesFileTypeAndParent(t *testing.T) {
	image := newFixtureImage(5, 0)
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, []fixtureEntry{
		{name: "sub", inodeNumber: 40, fileType: DirEntryFileTypeDirectory},
		{name: "file", inodeNumber: 41, fileType: DirEntryFileTypeRegularFile},
	})

	volume := image.open(t)
	entries, err := volume.ListRootDirectoryEntries()
	if err != nil {
		t.Fatalf("ListRootDirectoryEntries failed: %v", err)
	}
	assertNames(t, entries, "sub", "file")

	if entries[0].FileType != DirEntryFileTypeDirectory {
		t.Fatalf("expected directory ftype, got %d", entries[0].FileType)
	}
	if entries[1].FileType != DirEntryFileTypeRegularFile {
		t.Fatalf("expected regular file ftype, got %d", entries[1].FileType)
	}
	if DirEntryFileTypeName(entries[1].FileType) != "regular_file" {
		t.Fatalf("unexpected ftype name: %q", DirEntryFileTypeName(entries[1].FileType))
	}
}

// writeRawDirectoryEntry lays down an entry structure at an arbitrary offset,
// bypassing block framing, to simulate a stale record inside reclaimed space.
func writeRawDirectoryEntry(block []byte, offset int, entry fixtureEntry, hasFileType bool) {
	recordLength := fixtureEntryLength(len(entry.name), hasFileType)
	binary.BigEndian.PutUint64(block[offset:offset+8], entry.inodeNumber)
	block[offset+8] = byte(len(entry.name))
	copy(block[offset+9:], entry.name)
	if hasFileType {
		block[offset+9+len(entry.name)] = entry.fileType
	}
	binary.BigEndian.PutUint16(block[offset+recordLength-2:offset+recordLength], uint16(offset))
}

func hasReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func hasAnomaly(anomalies []ReportAnomaly, code string) bool {
	for _, anomaly := range anomalies {
		if anomaly.Code == code {
			return true
		}
	}
	return false
}
