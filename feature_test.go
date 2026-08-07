package libxfs

import (
	"errors"
	"testing"
	"time"
)

// referenceTime is a fixed, unambiguous instant used to prove that timestamps
// survive an encode/decode round trip.
var referenceTime = time.Date(2026, time.April, 20, 1, 57, 23, 430909438, time.UTC)

// TestBigTimeTimestampsDecoded is the regression test for bigtime timestamps.
//
// XFS stores timestamps either as a legacy 32-bit seconds/nanoseconds pair or,
// with the bigtime feature, as a single 64-bit nanosecond counter based at
// 1901-12-13 20:45:52 UTC. Decoding a bigtime value as a legacy pair yields a
// date roughly 27 years early — plausible enough to pass unnoticed, and wrong
// in the one field forensic work depends on most.
func TestBigTimeTimestampsDecoded(t *testing.T) {
	image := newFixtureImage(5, 0)
	image.setFeaturesIncompat(FeatureIncompatFileType | FeatureIncompatBigTime)
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, []fixtureEntry{
		{name: "file", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile},
	})
	image.writeInodeTimestamps(fixtureRootInode, referenceTime.UnixNano())

	volume := image.open(t)
	if !volume.Superblock().HasBigTimestamps() {
		t.Fatal("superblock did not report the bigtime feature")
	}

	inode, err := volume.OpenInode(fixtureRootInode)
	if err != nil {
		t.Fatalf("OpenInode failed: %v", err)
	}
	if !inode.HasBigTimestamps {
		t.Fatal("inode did not record that bigtime decoding was used")
	}

	for _, tc := range []struct {
		name string
		got  time.Time
	}{
		{"access", inode.AccessTime()},
		{"modification", inode.ModificationTime()},
		{"change", inode.InodeChangeTime()},
		{"creation", inode.CreationTime()},
	} {
		if !tc.got.Equal(referenceTime) {
			t.Fatalf("%s time mismatch: got %s want %s", tc.name, tc.got, referenceTime)
		}
	}
}

// TestLegacyTimestampsDecoded guards the other side of the branch: a v5
// filesystem without bigtime must still use the legacy encoding.
func TestLegacyTimestampsDecoded(t *testing.T) {
	image := newFixtureImage(5, 0)
	image.setFeaturesIncompat(FeatureIncompatFileType)
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, []fixtureEntry{
		{name: "file", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile},
	})
	image.writeInodeTimestamps(fixtureRootInode, referenceTime.UnixNano())

	volume := image.open(t)
	if volume.Superblock().HasBigTimestamps() {
		t.Fatal("bigtime reported without the feature bit")
	}

	inode, err := volume.OpenInode(fixtureRootInode)
	if err != nil {
		t.Fatalf("OpenInode failed: %v", err)
	}
	if inode.HasBigTimestamps {
		t.Fatal("inode used bigtime decoding without the feature bit")
	}
	if !inode.ModificationTime().Equal(referenceTime) {
		t.Fatalf("legacy time mismatch: got %s want %s", inode.ModificationTime(), referenceTime)
	}
}

// TestLargeExtentCountsDecoded is the regression test for the nrext64 feature,
// which moves the data fork extent counter to a 64-bit field at offset 24 and
// widens the attribute fork counter to 32 bits at offset 76. Reading the old
// offsets yields the attribute count where the data count belongs.
func TestLargeExtentCountsDecoded(t *testing.T) {
	image := newFixtureImage(5, 0)
	image.setFeaturesIncompat(FeatureIncompatFileType | FeatureIncompatLargeExtentCounts)

	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile})
	block := image.buildDirectoryBlock(image.directoryBlockMagic(), entries)
	image.addBlockDirectory(fixtureFirstInode, [][]byte{block})
	// A distinct attribute count: reading the pre-nrext64 offset would report
	// this value as the data fork extent count.
	image.setAttributeExtentCount(fixtureFirstInode, 7)
	linkRoot(image, fixtureFirstInode)

	volume := image.open(t)
	if !volume.Superblock().HasLargeExtentCounts() {
		t.Fatal("superblock did not report the nrext64 feature")
	}

	inode, err := volume.OpenInode(fixtureFirstInode)
	if err != nil {
		t.Fatalf("OpenInode failed: %v", err)
	}
	if inode.DataExtentCount != 1 {
		t.Fatalf("data extent count mismatch: got %d want 1", inode.DataExtentCount)
	}
	if inode.AttributeExtentCount != 7 {
		t.Fatalf("attribute extent count mismatch: got %d want 7", inode.AttributeExtentCount)
	}

	got, err := volume.ListDirectoryEntries(fixtureFirstInode)
	if err != nil {
		t.Fatalf("listing failed: %v", err)
	}
	assertNames(t, got, "alpha")
}

// TestUnknownIncompatFeatureRejected checks that an image using a layout this
// parser does not understand is refused rather than silently misread.
func TestUnknownIncompatFeatureRejected(t *testing.T) {
	image := newFixtureImage(5, 0)
	image.setFeaturesIncompat(FeatureIncompatFileType | (1 << 20))
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, nil)

	if _, err := Open(&mockReaderAt{data: image.data}); !errors.Is(err, ErrUnsupportedFeatureFlag) {
		t.Fatalf("expected ErrUnsupportedFeatureFlag, got %v", err)
	}
}

// TestV4SuperblockIgnoresFeatureWords checks that the v5-only feature words are
// not read on a v4 superblock, where that space holds unrelated data.
func TestV4SuperblockIgnoresFeatureWords(t *testing.T) {
	image := newFixtureImage(4, 0)
	// Write bytes where the v5 feature words would live.
	for i := 208; i < 224; i++ {
		image.data[i] = 0xff
	}
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, []fixtureEntry{
		{name: "file", inodeNumber: 40},
	})

	volume := image.open(t)
	sb := volume.Superblock()
	if sb.FeaturesIncompat != 0 || sb.HasBigTimestamps() || sb.HasLargeExtentCounts() {
		t.Fatalf("v4 superblock reported v5 feature words: %+v", sb)
	}
}

// TestXattrNamespaceNames pins the namespace strings to the names Linux uses,
// which is what downstream tools match on.
func TestXattrNamespaceNames(t *testing.T) {
	for _, tc := range []struct {
		flags     uint8
		prefix    string
		namespace string
	}{
		{0, "user.", "user"},
		{2, "trusted.", "trusted"},
		{4, "security.", "security"},
	} {
		prefix, namespace, err := xattrNamespaceFromFlags(tc.flags)
		if err != nil {
			t.Fatalf("flags %d: unexpected error: %v", tc.flags, err)
		}
		if prefix != tc.prefix || namespace != tc.namespace {
			t.Fatalf("flags %d: got %q/%q want %q/%q", tc.flags, prefix, namespace, tc.prefix, tc.namespace)
		}
	}
}

func TestBigTimeConversionBoundaries(t *testing.T) {
	// The bigtime epoch itself maps to the minimum legacy timestamp.
	if got := bigTimeToUnixNanos(0); got != -xfsBigTimeEpochOffsetSeconds*1_000_000_000 {
		t.Fatalf("bigtime zero mismatch: got %d", got)
	}
	// A counter beyond what int64 nanoseconds can express must clamp, not wrap.
	if got := bigTimeToUnixNanos(^uint64(0)); got != 1<<63-1 {
		t.Fatalf("expected saturation, got %d", got)
	}
}
