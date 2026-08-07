package libxfs

import (
	"sync"
	"testing"
)

// Concurrency tests.
//
// Volume advertises concurrency-safe reads, so the guarantees have to be
// exercised rather than assumed. These run under -race in CI.

// concurrencyFixture builds a small volume with a multi-block directory and a
// handful of inodes to read.
func concurrencyFixture(t testing.TB) *fixtureImage {
	t.Helper()

	image := newFixtureImage(5, 0)
	first := append(dotEntries(fixtureFirstInode, fixtureRootInode), namedEntries("a", 20, 100)...)
	blocks := [][]byte{
		image.buildDirectoryBlock(image.directoryDataMagic(), first),
		image.buildDirectoryBlock(image.directoryDataMagic(), namedEntries("b", 20, 200)),
	}
	image.addBlockDirectory(fixtureFirstInode, blocks)
	linkRoot(image, fixtureFirstInode)
	return image
}

// TestConcurrentReadsAreSafe exercises the documented guarantee: many readers
// on one Volume.
func TestConcurrentReadsAreSafe(t *testing.T) {
	volume := concurrencyFixture(t).open(t)

	const readers = 16
	const iterations = 50

	var group sync.WaitGroup
	errs := make(chan error, readers*iterations)

	for i := 0; i < readers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for j := 0; j < iterations; j++ {
				if _, err := volume.ListDirectoryEntries(fixtureFirstInode); err != nil {
					errs <- err
					return
				}
				if _, err := volume.OpenInode(fixtureRootInode); err != nil {
					errs <- err
					return
				}
				if _, err := volume.ResolveInodeByPath("/dir/b17"); err != nil {
					errs <- err
					return
				}
			}
		}()
	}

	group.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read failed: %v", err)
	}
}

// TestCloseDuringConcurrentReads is the important one.
//
// Close nils the inode cache while readers may still be inside OpenInode.
// Assignment to a nil map panics even while holding the mutex, and the
// IsClosed check at the top of OpenInode cannot prevent it: Close can land
// between that check and the cache write. Readers must observe ErrVolumeClosed,
// never a panic and never a read against a closed file handle.
func TestCloseDuringConcurrentReads(t *testing.T) {
	for attempt := 0; attempt < 50; attempt++ {
		volume := concurrencyFixture(t).open(t)

		var group sync.WaitGroup
		start := make(chan struct{})

		for i := 0; i < 8; i++ {
			group.Add(1)
			go func() {
				defer group.Done()
				<-start
				for j := 0; j < 100; j++ {
					// Any error is acceptable; a panic is not.
					_, _ = volume.OpenInode(fixtureRootInode)
					_, _ = volume.ListDirectoryEntries(fixtureFirstInode)
					_, _ = volume.ReadFileData(fixtureRootInode)
				}
			}()
		}

		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_ = volume.Close()
		}()

		close(start)
		group.Wait()
	}
}

// TestReadAfterCloseReportsClosed pins the post-close contract.
func TestReadAfterCloseReportsClosed(t *testing.T) {
	volume := concurrencyFixture(t).open(t)
	if err := volume.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if _, err := volume.OpenInode(fixtureRootInode); err != ErrVolumeClosed {
		t.Fatalf("OpenInode after close: got %v, want ErrVolumeClosed", err)
	}
	if _, err := volume.ListDirectoryEntries(fixtureFirstInode); err != ErrVolumeClosed {
		t.Fatalf("ListDirectoryEntries after close: got %v, want ErrVolumeClosed", err)
	}
	if _, err := volume.ReadFileData(fixtureRootInode); err != ErrVolumeClosed {
		t.Fatalf("ReadFileData after close: got %v, want ErrVolumeClosed", err)
	}
	if err := volume.Close(); err != ErrVolumeClosed {
		t.Fatalf("double Close: got %v, want ErrVolumeClosed", err)
	}
}

// TestCachedInodesAreNotSharedMutableState checks that a caller cannot mutate
// another caller's view of an inode through the shared cache.
func TestCachedInodesAreNotSharedMutableState(t *testing.T) {
	volume := concurrencyFixture(t).open(t)

	first, err := volume.OpenInode(fixtureRootInode)
	if err != nil {
		t.Fatalf("OpenInode failed: %v", err)
	}
	originalSize := first.Size
	originalExtents := len(first.DataExtents)

	// A caller mutating what it was handed must not corrupt the cache.
	first.Size = 0xdeadbeef
	first.DataExtents = nil
	if len(first.InlineData) > 0 {
		first.InlineData[0] ^= 0xff
	}

	second, err := volume.OpenInode(fixtureRootInode)
	if err != nil {
		t.Fatalf("second OpenInode failed: %v", err)
	}
	if second.Size != originalSize {
		t.Fatalf("cache was corrupted by a caller: size %d, want %d", second.Size, originalSize)
	}
	if len(second.DataExtents) != originalExtents {
		t.Fatalf("cache was corrupted by a caller: %d extents, want %d",
			len(second.DataExtents), originalExtents)
	}
}
