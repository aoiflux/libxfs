package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/aoiflux/libxfs"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <inode_number>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./xattrs disk.img 128")
		fmt.Println("  ./xattrs \\\\.\\C: 128")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	inodeNumber, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil {
		log.Fatalf("Invalid inode number %q: %v", os.Args[2], err)
	}

	file, err := os.Open(volumePath)
	if err != nil {
		log.Fatalf("Failed to open volume: %v", err)
	}
	defer file.Close()

	volume, err := libxfs.Open(file)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	attrs, err := volume.ListInodeExtendedAttributes(inodeNumber)
	if err != nil {
		log.Fatalf("Failed to list inode attributes: %v", err)
	}

	fmt.Printf("Inode %d attributes: %d\n", inodeNumber, len(attrs))
	for i, attr := range attrs {
		fmt.Printf("%d. %s\n", i+1, attr.Name)
		fmt.Printf("   Namespace: %s\n", attr.Namespace)
		fmt.Printf("   Flags: 0x%02x\n", attr.Flags)
		fmt.Printf("   Value Size: %d\n", len(attr.Value))
		if len(attr.Value) > 0 {
			preview := attr.Value
			if len(preview) > 32 {
				preview = preview[:32]
			}
			fmt.Printf("   Value Preview (hex):")
			for _, b := range preview {
				fmt.Printf(" %02x", b)
			}
			if len(attr.Value) > len(preview) {
				fmt.Print(" ...")
			}
			fmt.Println()
		}
	}
}
