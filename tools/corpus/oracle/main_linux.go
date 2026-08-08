//go:build linux

// Command oracle walks a mounted filesystem and emits a canonical record for
// every path it contains.
//
// This is the authoritative ground truth for libxfs's directory walk: it is
// produced by the kernel's own XFS implementation, so it shares no code and no
// assumptions with the library under test. st_ino on XFS is the on-disk inode
// number, which makes it a direct join key against libxfs output.
//
// Output is NDJSON sorted by path, so two runs of the same image are
// byte-identical and can be diffed with ordinary tools.
//
//	oracle -root /mnt/x > oracle-mount.ndjson
package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"syscall"
)

// Record is one filesystem object. Field names match the differ's expectations
// in oracle_diff_test.go.
type Record struct {
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

func main() {
	root := flag.String("root", "", "mountpoint to walk")
	hash := flag.Bool("hash", true, "digest regular file contents")
	flag.Parse()

	if *root == "" {
		fatal(fmt.Errorf("-root is required"))
	}

	records, err := walk(*root, *hash)
	if err != nil {
		fatal(err)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Path < records[j].Path })

	out := bufio.NewWriter(os.Stdout)
	defer out.Flush()
	encoder := json.NewEncoder(out)
	for _, record := range records {
		if err := encoder.Encode(record); err != nil {
			fatal(err)
		}
	}
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "oracle: %v\n", err)
	os.Exit(1)
}

// walk descends the tree without ever following a symlink, so that a symlink
// to a directory cannot introduce a path that XFS does not actually contain.
func walk(root string, hash bool) ([]Record, error) {
	rootInfo, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	rootRecord, err := describe(root, "/", rootInfo, hash)
	if err != nil {
		return nil, err
	}

	records := []Record{rootRecord}
	pending := []string{"/"}

	for len(pending) > 0 {
		logical := pending[len(pending)-1]
		pending = pending[:len(pending)-1]

		entries, err := os.ReadDir(path.Join(root, logical))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", logical, err)
		}
		for _, entry := range entries {
			childLogical := path.Join(logical, entry.Name())
			info, err := os.Lstat(path.Join(root, childLogical))
			if err != nil {
				return nil, fmt.Errorf("lstat %s: %w", childLogical, err)
			}
			record, err := describe(path.Join(root, childLogical), childLogical, info, hash)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
			if info.IsDir() {
				pending = append(pending, childLogical)
			}
		}
	}
	return records, nil
}

func describe(realPath, logicalPath string, info fs.FileInfo, hash bool) (Record, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Record{}, fmt.Errorf("%s: no stat data", logicalPath)
	}

	record := Record{
		Path:    logicalPath,
		Ino:     stat.Ino,
		Kind:    kindOf(info.Mode()),
		Size:    uint64(info.Size()),
		Nlink:   uint64(stat.Nlink),
		Mode:    uint32(info.Mode().Perm()),
		MtimeNs: stat.Mtim.Sec*1e9 + stat.Mtim.Nsec,
	}

	switch record.Kind {
	case "symlink":
		target, err := os.Readlink(realPath)
		if err != nil {
			return Record{}, fmt.Errorf("readlink %s: %w", logicalPath, err)
		}
		record.Target = target
		// A symlink's st_size is its target length; recording both makes a
		// mismatch between them obvious rather than silently consistent.
		record.Size = uint64(len(target))
	case "reg":
		if hash {
			digest, err := digestFile(realPath)
			if err != nil {
				return Record{}, err
			}
			record.SHA256 = digest
		}
	}
	return record, nil
}

func kindOf(mode fs.FileMode) string {
	switch {
	case mode&fs.ModeSymlink != 0:
		return "symlink"
	case mode.IsDir():
		return "dir"
	case mode&fs.ModeNamedPipe != 0:
		return "fifo"
	case mode&fs.ModeSocket != 0:
		return "socket"
	case mode&fs.ModeCharDevice != 0:
		return "chardev"
	case mode&fs.ModeDevice != 0:
		return "blockdev"
	case mode.IsRegular():
		return "reg"
	default:
		return strings.TrimSpace(mode.String())
	}
}

func digestFile(realPath string) (string, error) {
	file, err := os.Open(realPath)
	if err != nil {
		return "", err
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", fmt.Errorf("digest %s: %w", realPath, err)
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
