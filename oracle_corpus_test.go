package libxfs

// Loading of externally produced ground truth for the corpus images built by
// tools/corpus/mkcorpus.sh and described by tools/corpus/mkoracle.sh.
//
// Nothing in this file may use libxfs to derive a fact. Every value here comes
// either from the kernel (Oracle A, via a read-only mount) or from xfs_db
// (Oracle B, reading the image directly). Deriving ground truth with the
// library under test would make the whole exercise circular.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// corpusEnvVar points at the directory mkcorpus.sh wrote, one subdirectory per
// case. Tests skip when it is unset so the default `go test` run stays
// self-contained and platform independent.
const corpusEnvVar = "LIBXFS_CORPUS"

// oracleRecord is one filesystem object as the Linux kernel reported it.
// Fields mirror tools/corpus/oracle/main_linux.go.
type oracleRecord struct {
	Path    string `json:"path"`
	Ino     uint64 `json:"ino"`
	Kind    string `json:"kind"`
	Size    uint64 `json:"size"`
	Nlink   uint64 `json:"nlink"`
	Mode    uint32 `json:"mode"`
	MtimeNs int64  `json:"mtime_ns"`
	Target  string `json:"target,omitempty"`
	SHA256  string `json:"sha256,omitempty"`
}

// Directory index formats, as determined from the on-disk block map rather
// than from anything libxfs reports.
const (
	oracleIndexShortForm = "short_form"
	oracleIndexBlock     = "block"
	oracleIndexLeaf      = "leaf"
	oracleIndexNode      = "node"
)

// oracleDirectory is the true on-disk shape of one directory.
//
// Mapping format (how the data fork's extents are stored) and index format
// (how directory entries are indexed) are independent axes, and a corpus that
// does not cover both is not covering the format space.
type oracleDirectory struct {
	Ino           uint64
	Path          string
	MappingFormat string // local, extents, btree
	IndexFormat   string // short_form, block, leaf, node
	Size          uint64
	NumExtents    uint64
	DataBlocks    int
	LeafBlocks    int
	FreeBlocks    int
}

// corpusCase is one image plus everything known to be true about it.
type corpusCase struct {
	Name      string
	Dir       string
	ImagePath string

	RootInode uint64
	BlockSize uint64
	// DirectoryBlockSize is sb_blocksize << sb_dirblklog. mkfs.xfs raises it
	// above the filesystem block size on small-block filesystems, so directory
	// geometry cannot be derived from BlockSize alone.
	DirectoryBlockSize uint64

	// MountOracle is keyed by absolute path. Empty when the image could not be
	// mounted (v4 images on kernels that refuse them), in which case only
	// Oracle B is available.
	MountOracle map[string]oracleRecord
	// NcheckPaths is Oracle B's path set, keyed by absolute path.
	NcheckPaths map[string]uint64
	// Directories is keyed by inode number.
	Directories map[uint64]oracleDirectory

	// DamagedImagePath and DamagedDirectory describe a deliberately corrupted
	// copy of this case's image, when one exists. The oracle always describes
	// the intact original, because a damaged image has no ground truth.
	DamagedImagePath  string
	DamagedDirectory  uint64
	DamagedBlockIndex uint64

	// DeletedNames lists entries that were removed from this image while it
	// was mounted. The oracle describes the image after the deletions, so
	// these names are exactly what should no longer be reported as present,
	// and are the only names a carver could legitimately recover.
	DeletedNames []string
}

func (c corpusCase) hasDamagedImage() bool { return c.DamagedImagePath != "" }

func (c corpusCase) hasMountOracle() bool { return len(c.MountOracle) > 0 }

// expectedPaths is the set of paths the image is known to contain.
//
// It prefers the kernel's view, which carries full attributes. When the image
// cannot be mounted -- a v4 filesystem on a kernel built without
// CONFIG_XFS_SUPPORT_V4, which is the format a forensic library is most likely
// to be handed -- it falls back to xfs_db's name table. That yields paths and
// inode numbers but no sizes or kinds, so those assertions are skipped rather
// than silently compared against zero.
func (c corpusCase) expectedPaths() map[string]oracleRecord {
	if c.hasMountOracle() {
		return c.MountOracle
	}
	expected := make(map[string]oracleRecord, len(c.NcheckPaths))
	for path, inode := range c.NcheckPaths {
		expected[path] = oracleRecord{Path: path, Ino: inode}
	}
	return expected
}

// attributesKnown reports whether expectedPaths carries kinds and sizes.
func (c corpusCase) attributesKnown() bool { return c.hasMountOracle() }

// directoryFormatOf reports the true index format of the directory holding the
// given path, which is what allows a missing entry to be attributed to a
// specific directory format.
func (c corpusCase) directoryFormatOf(dirInode uint64) string {
	if directory, ok := c.Directories[dirInode]; ok {
		return directory.IndexFormat
	}
	return "unknown"
}

// loadCorpus discovers every case under LIBXFS_CORPUS.
func loadCorpus(t *testing.T) []corpusCase {
	t.Helper()

	root := os.Getenv(corpusEnvVar)
	if root == "" {
		t.Skipf("%s not set; skipping corpus tests (build one with tools/corpus/mkcorpus.sh)", corpusEnvVar)
	}

	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("reading corpus root %s: %v", root, err)
	}

	var cases []corpusCase
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		if _, err := os.Stat(filepath.Join(dir, "image.img")); err != nil {
			continue
		}
		// A case with no sb.txt has an image but no ground truth yet, which
		// happens while a corpus is still being built. Judging libxfs against
		// a half-written oracle would be worse than not judging it at all.
		if _, err := os.Stat(filepath.Join(dir, "sb.txt")); err != nil {
			t.Logf("case %s has no oracle yet; run tools/corpus/mkoracle.sh %s", entry.Name(), entry.Name())
			continue
		}
		loaded, err := loadCorpusCase(entry.Name(), dir)
		if err != nil {
			t.Fatalf("loading case %s: %v", entry.Name(), err)
		}
		cases = append(cases, loaded)
	}
	if len(cases) == 0 {
		t.Fatalf("no corpus cases found under %s", root)
	}
	sort.Slice(cases, func(i, j int) bool { return cases[i].Name < cases[j].Name })
	return cases
}

func loadCorpusCase(name, dir string) (corpusCase, error) {
	loaded := corpusCase{
		Name:      name,
		Dir:       dir,
		ImagePath: filepath.Join(dir, "image.img"),
	}

	superblock, err := parseOracleSuperblock(filepath.Join(dir, "sb.txt"))
	if err != nil {
		return loaded, err
	}
	loaded.RootInode = superblock["rootino"]
	loaded.BlockSize = superblock["blocksize"]
	if loaded.RootInode == 0 || loaded.BlockSize == 0 {
		return loaded, fmt.Errorf("sb.txt did not yield rootino and blocksize")
	}
	loaded.DirectoryBlockSize = loaded.BlockSize << superblock["dirblklog"]

	if loaded.MountOracle, err = parseMountOracle(filepath.Join(dir, "oracle-mount.ndjson")); err != nil {
		return loaded, err
	}
	if loaded.NcheckPaths, err = parseNcheck(filepath.Join(dir, "ncheck.txt"), loaded.RootInode); err != nil {
		return loaded, err
	}
	if loaded.Directories, err = parseDirInfo(filepath.Join(dir, "dirinfo.txt"),
		loaded.BlockSize, loaded.DirectoryBlockSize); err != nil {
		return loaded, err
	}

	damagedImage := filepath.Join(dir, "image.damaged.img")
	if _, err := os.Stat(damagedImage); err == nil {
		description, err := parseOracleSuperblock(filepath.Join(dir, "damaged.txt"))
		if err != nil {
			return loaded, err
		}
		loaded.DamagedImagePath = damagedImage
		loaded.DamagedDirectory = description["inode"]
		loaded.DamagedBlockIndex = description["logical_directory_block"]
	}

	if names, err := os.ReadFile(filepath.Join(dir, "deleted-names.txt")); err == nil {
		for _, name := range strings.Split(string(names), "\n") {
			if name = strings.TrimSpace(name); name != "" {
				loaded.DeletedNames = append(loaded.DeletedNames, name)
			}
		}
	}
	return loaded, nil
}

// parseOracleSuperblock reads the "name = value" lines xfs_db prints for the
// superblock. Values may be decimal or 0x-prefixed.
func parseOracleSuperblock(path string) (map[string]uint64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	values := map[string]uint64{}
	for _, line := range strings.Split(string(data), "\n") {
		key, raw, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		key = strings.TrimSpace(key)
		raw = strings.TrimSpace(raw)
		if strings.HasPrefix(raw, "0x") {
			if parsed, err := strconv.ParseUint(raw[2:], 16, 64); err == nil {
				values[key] = parsed
			}
			continue
		}
		if parsed, err := strconv.ParseUint(raw, 10, 64); err == nil {
			values[key] = parsed
		}
	}
	return values, nil
}

func parseMountOracle(path string) (map[string]oracleRecord, error) {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		// The image could not be mounted; Oracle B still applies.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()

	records := map[string]oracleRecord{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var record oracleRecord
		if err := json.Unmarshal(line, &record); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		records[record.Path] = record
	}
	return records, scanner.Err()
}

// parseNcheck reads xfs_db's inode-to-name table.
//
// Lines are "<inode> <path>", where the path is relative to the root and a
// directory is named with a trailing "/.". Paths may contain spaces and tabs,
// so everything after the leading number is taken verbatim. The root itself is
// never listed, because it has no parent entry naming it.
func parseNcheck(path string, rootInode uint64) (map[string]uint64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	paths := map[string]uint64{"/": rootInode}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimLeft(scanner.Text(), " \t")
		if line == "" {
			continue
		}
		number, rest, found := strings.Cut(line, " ")
		if !found {
			continue
		}
		inode, err := strconv.ParseUint(number, 10, 64)
		if err != nil {
			continue
		}
		relative := strings.TrimLeft(rest, " \t")
		relative = strings.TrimSuffix(relative, "/.")
		paths["/"+relative] = inode
	}
	return paths, scanner.Err()
}

// parseDirInfo reads the per-directory dump mkoracle.sh produced: a "## ino N
// path P" marker, xfs_db's inode core fields, then the logical block map.
//
// The index format is derived from where the directory's blocks live. XFS
// divides a directory's logical space into 32 GiB regions: data blocks in the
// first, leaf and node index blocks in the second, free-space bitmaps in the
// third. A directory with no blocks in the index region is in block format;
// one with a single index block is in leaf format; anything more needs a
// da-node above the leaves.
func parseDirInfo(path string, blockSize, directoryBlockSize uint64) (map[uint64]oracleDirectory, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if blockSize == 0 || directoryBlockSize == 0 {
		return nil, fmt.Errorf("%s: block geometry is unknown", path)
	}

	const directorySpaceSize = uint64(1) << 35
	leafRegionStart := directorySpaceSize / blockSize
	freeRegionStart := 2 * directorySpaceSize / blockSize

	directories := map[uint64]oracleDirectory{}
	var current *oracleDirectory

	// The index occupies whole directory blocks, which on a small-block
	// filesystem span several filesystem blocks. Counting in filesystem blocks
	// would call a single-leaf-block directory node format.
	blocksPerDirectoryBlock := directoryBlockSize / blockSize

	flush := func() {
		if current == nil {
			return
		}
		current.IndexFormat = classifyOracleIndexFormat(*current, blocksPerDirectoryBlock)
		directories[current.Ino] = *current
		current = nil
	}

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimRight(line, "\r")

		if strings.HasPrefix(line, "## ino ") {
			flush()
			rest := strings.TrimPrefix(line, "## ino ")
			number, directoryPath, _ := strings.Cut(rest, " path ")
			inode, err := strconv.ParseUint(strings.TrimSpace(number), 10, 64)
			if err != nil {
				return nil, fmt.Errorf("%s: bad marker %q", path, line)
			}
			current = &oracleDirectory{Ino: inode, Path: directoryPath}
			continue
		}
		if current == nil {
			continue
		}

		if key, raw, found := strings.Cut(line, "="); found {
			value := strings.TrimSpace(raw)
			switch strings.TrimSpace(key) {
			case "core.format":
				// Printed as "2 (extents)"; the word is the useful part.
				if open := strings.Index(value, "("); open >= 0 {
					current.MappingFormat = strings.Trim(value[open:], "()")
				}
			case "core.size":
				current.Size, _ = strconv.ParseUint(value, 10, 64)
			case "core.nextents":
				current.NumExtents, _ = strconv.ParseUint(value, 10, 64)
			}
			continue
		}

		// "data offset 0 startblock 15 (0/15) count 1 flag 0"
		if strings.HasPrefix(line, "data offset ") {
			offset, count, ok := parseBmapLine(line)
			if !ok {
				return nil, fmt.Errorf("%s: unparsable block map line %q", path, line)
			}
			switch {
			case offset >= freeRegionStart:
				current.FreeBlocks += count
			case offset >= leafRegionStart:
				current.LeafBlocks += count
			default:
				current.DataBlocks += count
			}
		}
	}
	flush()
	return directories, nil
}

func classifyOracleIndexFormat(directory oracleDirectory, blocksPerDirectoryBlock uint64) string {
	if blocksPerDirectoryBlock == 0 {
		blocksPerDirectoryBlock = 1
	}
	leafDirectoryBlocks := uint64(directory.LeafBlocks) / blocksPerDirectoryBlock

	switch {
	case directory.MappingFormat == "local":
		return oracleIndexShortForm
	case directory.LeafBlocks == 0 && directory.FreeBlocks == 0:
		return oracleIndexBlock
	case leafDirectoryBlocks == 1 && directory.FreeBlocks == 0:
		return oracleIndexLeaf
	default:
		return oracleIndexNode
	}
}

// parseBmapLine extracts the logical offset and block count from an xfs_db
// "bmap -d" line, keyed by field name so that a change in xfs_db's spacing or
// extra columns does not silently shift the values.
func parseBmapLine(line string) (offset uint64, count int, ok bool) {
	fields := strings.Fields(line)
	var haveOffset, haveCount bool
	for i := 0; i+1 < len(fields); i++ {
		switch fields[i] {
		case "offset":
			if parsed, err := strconv.ParseUint(fields[i+1], 10, 64); err == nil {
				offset, haveOffset = parsed, true
			}
		case "count":
			if parsed, err := strconv.Atoi(fields[i+1]); err == nil {
				count, haveCount = parsed, true
			}
		}
	}
	return offset, count, haveOffset && haveCount
}
