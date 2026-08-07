package libxfs

import (
	"fmt"
	"os"
	"testing"
)

// openConformanceImageForBenchmark mirrors openConformanceImage for benchmarks,
// which take a *testing.B rather than a *testing.T.
func openConformanceImageForBenchmark(b *testing.B) *Volume {
	b.Helper()

	path := os.Getenv("LIBXFS_TEST_IMAGE")
	if path == "" {
		b.Skip("LIBXFS_TEST_IMAGE not set; skipping real-image benchmarks")
	}

	volume, err := OpenVolumeFromPath(path)
	if err != nil {
		b.Fatalf("failed to open %s: %v", path, err)
	}
	b.Cleanup(func() { _ = volume.Close() })
	return volume
}

// deepestImagePath returns the longest directory path in the image, so the
// resolution benchmark reflects the image rather than an assumed layout.
func deepestImagePath(b *testing.B, volume *Volume) string {
	b.Helper()

	type item struct {
		inode uint64
		path  string
	}
	root := volume.Superblock().RootDirectoryInodeNumber
	pending := []item{{root, ""}}
	visited := map[uint64]bool{}
	deepest := "/"

	for len(pending) > 0 {
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if visited[current.inode] {
			continue
		}
		visited[current.inode] = true
		if len(current.path) > len(deepest) {
			deepest = current.path
		}

		entries, err := volume.ListDirectoryEntries(current.inode)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.FileType != DirEntryFileTypeDirectory {
				continue
			}
			pending = append(pending, item{entry.InodeNumber, current.path + "/" + entry.Name})
		}
	}
	if deepest == "" {
		return "/"
	}
	return deepest
}

// Benchmarks guarding the directory walker against algorithmic regressions.
//
// Phase 3 (leaf/node hash lookup) is gated on the evidence these produce:
// path resolution currently lists a whole directory per path component, so the
// question is whether that cost is actually a problem at realistic sizes.

// buildWideDirectory creates a directory holding entryCount entries spread over
// as many directory blocks as they need.
func buildWideDirectory(tb testing.TB, entryCount int) (*Volume, uint64) {
	tb.Helper()

	image := newFixtureImage(5, 0)

	var blocks [][]byte
	inode := uint64(1000)
	remaining := entryCount
	first := true

	for remaining > 0 {
		var entries []fixtureEntry
		if first {
			entries = append(entries, dotEntries(fixtureFirstInode, fixtureRootInode)...)
			first = false
		}
		// Fill the block, leaving room for the free run that follows.
		used := fixtureDir3DataHeaderSize
		for _, entry := range entries {
			used += fixtureEntryLength(len(entry.name), true)
		}
		for remaining > 0 {
			name := fmt.Sprintf("entry_%06d", inode)
			size := fixtureEntryLength(len(name), true)
			if used+size+8 > int(image.dirBlockSize) {
				break
			}
			entries = append(entries, fixtureEntry{
				name:        name,
				inodeNumber: inode,
				fileType:    DirEntryFileTypeRegularFile,
			})
			used += size
			inode++
			remaining--
		}
		blocks = append(blocks, image.buildDirectoryBlock(image.directoryDataMagic(), entries))
	}

	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)

	volume, err := Open(&mockReaderAt{data: image.data})
	if err != nil {
		tb.Fatalf("Open failed: %v", err)
	}
	return volume, fixtureFirstInode
}

func BenchmarkListLargeDirectory(b *testing.B) {
	for _, entryCount := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("entries=%d", entryCount), func(b *testing.B) {
			volume, inode := buildWideDirectory(b, entryCount)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				entries, err := volume.ListDirectoryEntries(inode)
				if err != nil {
					b.Fatalf("listing failed: %v", err)
				}
				if len(entries) != entryCount {
					b.Fatalf("expected %d entries, got %d", entryCount, len(entries))
				}
			}
		})
	}
}

// BenchmarkResolveEntryInLargeDirectory measures resolving a single name, the
// operation a hash index would accelerate. The worst case is the last entry,
// since the walker scans in on-disk order.
func BenchmarkResolveEntryInLargeDirectory(b *testing.B) {
	for _, entryCount := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("entries=%d", entryCount), func(b *testing.B) {
			volume, _ := buildWideDirectory(b, entryCount)
			target := fmt.Sprintf("/dir/entry_%06d", 1000+entryCount-1)

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := volume.ResolveInodeByPath(target); err != nil {
					b.Fatalf("resolve failed: %v", err)
				}
			}
		})
	}
}

// BenchmarkScanDirectoryRecords covers the forensic path, which additionally
// carves free space.
func BenchmarkScanDirectoryRecords(b *testing.B) {
	volume, inode := buildWideDirectory(b, 1000)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := volume.ScanDirectoryRecordsWithOptions(inode, DirectoryScanOptions{}); err != nil {
			b.Fatalf("scan failed: %v", err)
		}
	}
}

// BenchmarkImageResolveDeepPath measures path resolution on a real filesystem,
// where each component costs a directory read. Skipped without an image.
func BenchmarkImageResolveDeepPath(b *testing.B) {
	volume := openConformanceImageForBenchmark(b)

	// Find the deepest path in the image so the benchmark is representative
	// rather than assuming a layout.
	deepest := deepestImagePath(b, volume)
	b.Logf("resolving %s", deepest)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := volume.ResolveInodeByPath(deepest); err != nil {
			b.Fatalf("resolve failed: %v", err)
		}
	}
}

func BenchmarkImageFullTraversal(b *testing.B) {
	volume := openConformanceImageForBenchmark(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		root := volume.Superblock().RootDirectoryInodeNumber
		pending := []uint64{root}
		visited := map[uint64]bool{}
		for len(pending) > 0 {
			current := pending[len(pending)-1]
			pending = pending[:len(pending)-1]
			if visited[current] {
				continue
			}
			visited[current] = true

			entries, err := volume.ListDirectoryEntries(current)
			if err != nil {
				b.Fatalf("listing failed: %v", err)
			}
			for _, entry := range entries {
				if entry.FileType == DirEntryFileTypeDirectory {
					pending = append(pending, entry.InodeNumber)
				}
			}
		}
	}
}
