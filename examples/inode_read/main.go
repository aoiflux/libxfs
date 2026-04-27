package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"strconv"

	"github.com/aoiflux/libxfs"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <inode_number> [max_bytes]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./inode_read disk.img 128")
		fmt.Println("  ./inode_read disk.img 128 4096")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	inodeNumber, err := strconv.ParseUint(os.Args[2], 10, 64)
	if err != nil {
		log.Fatalf("Invalid inode number %q: %v", os.Args[2], err)
	}

	maxBytes := 1024
	if len(os.Args) >= 4 {
		v, err := strconv.Atoi(os.Args[3])
		if err != nil || v <= 0 {
			log.Fatalf("Invalid max_bytes %q", os.Args[3])
		}
		maxBytes = v
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

	inode, err := volume.OpenInode(inodeNumber)
	if err != nil {
		log.Fatalf("Failed to open inode %d: %v", inodeNumber, err)
	}

	fmt.Printf("Inode: %d\n", inodeNumber)
	fmt.Printf("Size: %d bytes\n", inode.Size)
	fmt.Printf("Directory: %v\n", inode.IsDirectory())
	fmt.Printf("Fork Type: %d\n", inode.ForkType)

	buf := make([]byte, maxBytes)
	n, err := volume.ReadInodeData(inodeNumber, buf, 0)
	if err != nil && err != io.EOF {
		log.Fatalf("Failed to read inode data: %v", err)
	}
	buf = buf[:n]

	fmt.Printf("\nRead %d byte(s) at offset 0\n", n)
	if n == 0 {
		fmt.Println("(no readable data)")
		return
	}

	const row = 16
	for i := 0; i < len(buf); i += row {
		end := i + row
		if end > len(buf) {
			end = len(buf)
		}
		fmt.Printf("%08x  ", i)
		for j := i; j < end; j++ {
			fmt.Printf("%02x ", buf[j])
		}
		fmt.Println()
	}
}
