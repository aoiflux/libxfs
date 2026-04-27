package main

import (
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aoiflux/libxfs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <xfs_volume_or_image>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./basic /dev/sda1         # Linux")
		fmt.Println("  ./basic \\\\.\\C:           # Windows (requires Administrator)")
		fmt.Println("  ./basic disk.img          # Disk image file")
		fmt.Println("\nNote: On Windows, raw drive access requires Administrator privileges")
		os.Exit(1)
	}

	volumePath := os.Args[1]

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	sb := volume.Superblock()
	fmt.Println("=== XFS Volume Information ===")
	fmt.Printf("Format Version: %d\n", sb.FormatVersion)
	fmt.Printf("Block Size: %d bytes\n", sb.BlockSize)
	fmt.Printf("Inode Size: %d bytes\n", sb.InodeSize)
	fmt.Printf("Sector Size: %d bytes\n", sb.SectorSize)
	fmt.Printf("Total Blocks: %d\n", sb.NumberOfBlocks)
	fmt.Printf("Allocation Groups: %d\n", sb.NumberOfAllocationGroups)
	fmt.Printf("Allocation Group Size: %d blocks\n", sb.AllocationGroupSize)
	fmt.Printf("Root Inode: %d\n", sb.RootDirectoryInodeNumber)
	fmt.Printf("Volume Label: %q\n", strings.TrimRight(string(sb.VolumeLabel[:]), "\x00"))
	fmt.Println()

	root, err := volume.GetRootInode()
	if err != nil {
		log.Fatalf("Failed to open root inode: %v", err)
	}

	fmt.Println("=== Root Inode ===")
	fmt.Printf("Directory: %v\n", root.IsDirectory())
	fmt.Printf("Size: %d bytes\n", root.Size)
	fmt.Printf("Data Fork Type: %d\n", root.ForkType)
	fmt.Printf("Data Extents: %d\n", len(root.DataExtents))
	fmt.Printf("Attribute Fork Type: %d\n", root.AttributesForkType)
	fmt.Printf("Attribute Extents: %d\n", len(root.AttributesExtents))
	fmt.Println()

	entries, err := volume.ListRootDirectoryEntries()
	if err != nil {
		fmt.Printf("=== Root Directory Entries ===\n")
		fmt.Printf("directory decode skipped: %v\n\n", err)
	} else {
		fmt.Println("=== Root Directory Entries ===")
		fmt.Printf("Total entries: %d\n", len(entries))
		for _, entry := range entries {
			if entry.Name == "." || entry.Name == ".." {
				continue
			}
			fmt.Printf("  %s (inode=%d)\n", entry.Name, entry.InodeNumber)
		}
		fmt.Println()
	}

	attrs, err := volume.ListInodeExtendedAttributes(sb.RootDirectoryInodeNumber)
	if err != nil {
		fmt.Printf("Extended attributes: error: %v\n", err)
		return
	}

	fmt.Printf("Extended attributes: %d\n", len(attrs))
	for _, attr := range attrs {
		fmt.Printf("  %s (%s) size=%d\n", attr.Name, attr.Namespace, len(attr.Value))
	}
}
