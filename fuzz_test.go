package libxfs

import (
	"testing"
)

// Fuzz targets for the parsers that consume attacker-controlled bytes.
//
// A forensic parser ingests hostile images by definition. The invariant for
// every target below is the same: never panic, never allocate without bound,
// always terminate. Correctness of the parse is asserted by the unit tests;
// these targets exist to prove the failure modes are graceful.

func FuzzParseDirectoryBlockRecords(f *testing.F) {
	image := newFixtureImage(5, 0)
	entries := append(dotEntries(fixtureFirstInode, fixtureRootInode),
		fixtureEntry{name: "alpha", inodeNumber: 40, fileType: DirEntryFileTypeRegularFile},
		fixtureEntry{name: "beta", inodeNumber: 41, fileType: DirEntryFileTypeDirectory},
	)
	f.Add(image.buildDirectoryBlock(image.directoryBlockMagic(), entries))
	f.Add(image.buildDirectoryBlock(image.directoryDataMagic(), entries))

	v4 := newFixtureImage(4, 0)
	f.Add(v4.buildDirectoryBlock(v4.directoryBlockMagic(), entries))
	f.Add(v4.buildDirectoryBlock(v4.directoryDataMagic(), entries))
	f.Add([]byte("XDD3"))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, block []byte) {
		for _, hasFileType := range []bool{true, false} {
			for _, bestEffort := range []bool{true, false} {
				records, anomalies, err := parseDirectoryBlockRecords(block, directoryParseContext{
					hasFileType:    hasFileType,
					includeDeleted: true,
					bestEffort:     bestEffort,
				})
				if err != nil {
					continue
				}
				for _, record := range records {
					// Every record must be self-describing: a caller has to be
					// able to tell fact from candidate without guessing.
					if record.Kind == "" {
						t.Fatalf("record without a kind: %+v", record)
					}
					if record.Kind == RecordKindCarved && !record.IsCarved {
						t.Fatalf("carved record not flagged: %+v", record)
					}
					if record.IsVerified() && record.IsProbabilistic() {
						t.Fatalf("record is both verified and probabilistic: %+v", record)
					}
					if int(record.Offset) > len(block) {
						t.Fatalf("record offset outside block: %+v", record)
					}
				}
				if !bestEffort && len(anomalies) != 0 {
					t.Fatalf("strict mode must not emit anomalies: %+v", anomalies)
				}
			}
		}
	})
}

func FuzzParseShortFormDirectoryEntries(f *testing.F) {
	f.Add([]byte{1, 0, 0, 0, 0, 32, 3, 0, 0, 'd', 'i', 'r', 2, 0, 0, 0, 33})
	f.Add([]byte{0, 1, 0, 0, 0, 0, 0, 0, 0, 32})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, inlineData []byte) {
		inode := &Inode{
			ForkType:   ForkTypeInlineData,
			InlineData: inlineData,
		}
		for _, formatVersion := range []uint8{4, 5} {
			entries, err := parseShortFormDirectoryEntries(inode, formatVersion, 0)
			if err != nil {
				continue
			}
			for _, entry := range entries {
				if len(entry.Name) > maxDirectoryNameBytes {
					t.Fatalf("name longer than the format allows: %d", len(entry.Name))
				}
			}
			// The header is also parsed on its own by DirectoryParentInode.
			if _, err := parseShortFormDirectoryHeader(inlineData); err != nil {
				t.Fatalf("header rejected but entries parsed: %v", err)
			}
		}
	})
}

func FuzzParseExtentList(f *testing.F) {
	extent := encodeExtent(0, 6, 4, false)
	f.Add(extent[:], uint32(1), uint64(4))
	f.Add(extent[:], uint32(64), uint64(4))
	f.Add([]byte{}, uint32(0), uint64(0))

	f.Fuzz(func(t *testing.T, data []byte, numberOfExtents uint32, numberOfBlocks uint64) {
		// Bound the declared count so the fuzzer spends its time on parsing
		// rather than on allocating a huge slice from a random uint32.
		if numberOfExtents > 4096 {
			numberOfExtents = 4096
		}
		for _, addSparse := range []bool{true, false} {
			extents, err := parseExtentList(numberOfBlocks, numberOfExtents, data, addSparse)
			if err != nil {
				continue
			}
			if len(extents) > 0 && numberOfExtents == 0 && !addSparse {
				t.Fatalf("extents produced from a zero count: %d", len(extents))
			}
		}
	})
}

func FuzzParseSuperblock(f *testing.F) {
	image := newFixtureImage(5, 0)
	f.Add(image.data[:superblockSize])
	f.Add(newFixtureImage(4, 0).data[:superblockSize])
	f.Add(make([]byte, superblockSize))

	f.Fuzz(func(t *testing.T, data []byte) {
		sb, err := parseSuperblock(data)
		if err != nil {
			return
		}
		if sb.BlockSize < minBlockSize || sb.BlockSize > maxBlockSize {
			t.Fatalf("accepted an out-of-range block size: %d", sb.BlockSize)
		}
		if sb.InodeSize < minInodeSize || sb.InodeSize > maxInodeSize {
			t.Fatalf("accepted an out-of-range inode size: %d", sb.InodeSize)
		}
		if sb.RelativeInodeNumberBits == 0 || sb.RelativeInodeNumberBits >= 32 {
			t.Fatalf("accepted unusable inode addressing: %d", sb.RelativeInodeNumberBits)
		}
		if sb.DirectoryBlockSize == 0 {
			t.Fatalf("accepted a zero directory block size")
		}
	})
}

func FuzzParseInode(f *testing.F) {
	image := newFixtureImage(5, 0)
	image.addShortFormDirectory(fixtureRootInode, fixtureRootInode, []fixtureEntry{
		{name: "dir", inodeNumber: fixtureFirstInode, fileType: DirEntryFileTypeDirectory},
	})
	start := fixtureRootInode * fixtureInodeSize
	f.Add(image.data[start:start+fixtureInodeSize], uint32(fixtureBlockSize))
	f.Add(make([]byte, fixtureInodeSize), uint32(fixtureBlockSize))

	f.Fuzz(func(t *testing.T, data []byte, blockSize uint32) {
		if blockSize == 0 {
			blockSize = fixtureBlockSize
		}
		inode, err := parseInode(data, blockSize, inodeFeatures{})
		if err != nil {
			return
		}
		if inode.ForkType == ForkTypeInlineData && uint64(len(inode.InlineData)) != inode.Size {
			t.Fatalf("inline data length %d does not match size %d", len(inode.InlineData), inode.Size)
		}
		if int(inode.DataForkOffset)+int(inode.DataForkSize) > len(data) {
			t.Fatalf("data fork extends past the inode: offset=%d size=%d len=%d",
				inode.DataForkOffset, inode.DataForkSize, len(data))
		}
	})
}

func FuzzParseBlockDirectoryEntriesEnd(f *testing.F) {
	f.Add(make([]byte, 4096), 16)
	f.Add(make([]byte, 64), 64)

	f.Fuzz(func(t *testing.T, block []byte, headerSize int) {
		if headerSize < 0 || headerSize > len(block) {
			return
		}
		end, err := blockDirectoryEntriesEnd(block, headerSize)
		if err != nil {
			return
		}
		if end < headerSize || end > len(block) {
			t.Fatalf("entry region [%d, %d) outside block of %d bytes", headerSize, end, len(block))
		}
	})
}
