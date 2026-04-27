package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/aoiflux/libxfs"
)

type stats struct {
	directories int
	files       int
}

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <start_folder_inode_or_path> [max_depth]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./traverse disk.img /")
		fmt.Println("  ./traverse disk.img 128")
		fmt.Println("  ./traverse disk.img /home 4")
		fmt.Println("\nNote: Path traversal currently supports short-form inline directories.")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	startSelector := os.Args[2]
	maxDepth := 5
	if len(os.Args) >= 4 {
		v, err := strconv.Atoi(os.Args[3])
		if err != nil || v < 0 {
			log.Fatalf("Invalid max_depth %q", os.Args[3])
		}
		maxDepth = v
	}

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	startInode, startPath, err := resolveStartSelector(volume, startSelector)
	if err != nil {
		log.Fatalf("Failed to resolve start folder %q: %v", startSelector, err)
	}

	fmt.Println("=== XFS Traverse ===")
	fmt.Printf("Start: %s (inode=%d)\n", startPath, startInode)
	fmt.Printf("Max depth: %d\n\n", maxDepth)

	visited := make(map[uint64]bool)
	st := &stats{}
	if err := traverse(volume, startInode, startPath, 0, maxDepth, visited, st); err != nil {
		log.Fatalf("Traversal failed: %v", err)
	}

	fmt.Println("\n=== Statistics ===")
	fmt.Printf("Directories: %d\n", st.directories)
	fmt.Printf("Files: %d\n", st.files)
}

func resolveStartSelector(volume *libxfs.Volume, selector string) (uint64, string, error) {
	if selector == "/" {
		sb := volume.Superblock()
		return sb.RootDirectoryInodeNumber, "/", nil
	}
	if n, err := strconv.ParseUint(selector, 10, 64); err == nil {
		return n, fmt.Sprintf("inode:%d", n), nil
	}
	if strings.HasPrefix(selector, "/") {
		inode, err := volume.ResolveInodeByPath(selector)
		if err != nil {
			return 0, "", err
		}
		return inode, selector, nil
	}
	return 0, "", fmt.Errorf("selector must be inode number or absolute path")
}

func traverse(volume *libxfs.Volume, inodeNumber uint64, path string, depth int, maxDepth int, visited map[uint64]bool, st *stats) error {
	if depth > maxDepth {
		return nil
	}
	if visited[inodeNumber] {
		fmt.Printf("%s[LOOP] %s (inode=%d)\n", strings.Repeat("  ", depth), path, inodeNumber)
		return nil
	}
	visited[inodeNumber] = true

	inode, err := volume.OpenInode(inodeNumber)
	if err != nil {
		return fmt.Errorf("open inode %d: %w", inodeNumber, err)
	}

	indent := strings.Repeat("  ", depth)
	if !inode.IsDirectory() {
		st.files++
		fmt.Printf("%s[FILE] %s (inode=%d size=%d)\n", indent, path, inodeNumber, inode.Size)
		return nil
	}

	st.directories++
	fmt.Printf("%s[DIR] %s (inode=%d fork=%d size=%d)\n", indent, path, inodeNumber, inode.ForkType, inode.Size)

	entries, err := volume.ListDirectoryEntries(inodeNumber)
	if err != nil {
		fmt.Printf("%s  (directory decode skipped: %v)\n", indent, err)
		return nil
	}

	for _, entry := range entries {
		if entry.Name == "." || entry.Name == ".." {
			continue
		}
		nextPath := path
		if nextPath == "/" {
			nextPath += entry.Name
		} else {
			nextPath += "/" + entry.Name
		}
		if err := traverse(volume, entry.InodeNumber, nextPath, depth+1, maxDepth, visited, st); err != nil {
			fmt.Printf("%s  (error: %v)\n", indent, err)
		}
	}

	return nil
}
