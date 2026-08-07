package libxfs

// Concurrency-safety layer for Volume.
//
// # Model
//
// A Volume is safe for concurrent use by multiple goroutines. All exported
// read APIs may be called in parallel on a single Volume, and Close may be
// called concurrently with them.
//
// Two pieces of state need protection, and they are guarded separately so that
// a long read never blocks an unrelated cache lookup:
//
//   - stateMu guards the volume's lifetime: the closed flag, the backing
//     reader, and the source closer. Every disk access takes it for reading,
//     so Close (which takes it for writing) waits for in-flight reads to
//     finish before releasing the file handle. Without this, Close could shut
//     the handle while a reader was inside it.
//   - inodeCacheMu guards the inode cache. A nil cache means "closed": writes
//     are dropped rather than panicking, because a reader can still be between
//     its closed check and its cache write when Close lands.
//
// Locks are never held across a call that reacquires them, so there is no
// recursive locking and no lock ordering to observe.
//
// # Data ownership
//
// OpenInode returns a copy of the cached inode, so a caller cannot corrupt
// another goroutine's view by assigning to a field. The slice fields
// (InlineData, DataExtents, AttributesExtents, InlineAttributesData, Raw)
// alias cached storage and must be treated as read-only; copy them before
// modifying.

// readAt reads len(buf) bytes at off from the backing reader.
//
// It holds the state lock for the duration, which is what prevents Close from
// releasing the reader underneath an in-flight read.
func (v *Volume) readAt(buf []byte, off int64) error {
	v.stateMu.RLock()
	defer v.stateMu.RUnlock()

	if v.closed {
		return ErrVolumeClosed
	}
	return readAtFull(v.reader, buf, off)
}

// lookupCachedInode returns a cached inode, or nil when absent or closed.
func (v *Volume) lookupCachedInode(inodeNumber uint64) *Inode {
	v.inodeCacheMu.RLock()
	defer v.inodeCacheMu.RUnlock()

	if v.inodeCache == nil {
		return nil
	}
	return v.inodeCache[inodeNumber]
}

// cacheInode stores a parsed inode.
//
// A nil cache means the volume was closed while this parse was in flight; the
// result is simply dropped. Writing to a nil map would panic, and the closed
// check in OpenInode cannot prevent that on its own because Close can land
// between the check and the write.
func (v *Volume) cacheInode(inodeNumber uint64, inode *Inode) {
	v.inodeCacheMu.Lock()
	defer v.inodeCacheMu.Unlock()

	if v.inodeCache == nil {
		return
	}
	v.inodeCache[inodeNumber] = inode
}

// clearInodeCache drops every cached inode and marks the cache unusable.
func (v *Volume) clearInodeCache() {
	v.inodeCacheMu.Lock()
	defer v.inodeCacheMu.Unlock()

	v.inodeCache = nil
}

// shallowCopyInode returns a copy of an inode so that callers cannot mutate
// cached state through the pointer they were handed.
//
// Slice fields intentionally alias the cached inode: copying them on every
// lookup would allocate on the hottest path in the library for no benefit,
// since nothing in this package mutates them and they are documented
// read-only.
func shallowCopyInode(inode *Inode) *Inode {
	if inode == nil {
		return nil
	}
	copied := *inode
	return &copied
}
