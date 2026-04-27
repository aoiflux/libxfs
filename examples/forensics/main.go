package main

import (
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/aoiflux/libxfs"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <directory_inode_or_path>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./forensics disk.img 128")
		fmt.Println("  ./forensics disk.img /carvey")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	target := os.Args[2]

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	records, targetDisplay, err := scanTarget(volume, target)
	if err != nil {
		log.Fatalf("Forensics scan failed for %q: %v", target, err)
	}

	sort.Slice(records, func(i, j int) bool {
		return records[i].Offset < records[j].Offset
	})

	fmt.Println("=== Directory Forensics ===")
	fmt.Printf("Target: %s\n", targetDisplay)
	fmt.Printf("Recovered records: %d\n\n", len(records))

	active := 0
	deleted := 0
	carved := 0

	for i, record := range records {
		kind := "active"
		if record.IsDeleted {
			kind = "deleted"
			deleted++
		} else {
			active++
		}
		if record.IsCarved {
			carved++
		}

		fmt.Printf("%d. [%s] name=%q inode=%d offset=%d len=%d carved=%v confidence=%s\n",
			i+1,
			kind,
			record.Name,
			record.InodeNumber,
			record.Offset,
			record.RecordLength,
			record.IsCarved,
			record.Confidence,
		)
	}

	fmt.Println()
	fmt.Printf("Active records: %d\n", active)
	fmt.Printf("Deleted records: %d\n", deleted)
	fmt.Printf("Carved deleted candidates: %d\n", carved)
}

func scanTarget(volume *libxfs.Volume, target string) ([]libxfs.DirectoryRecord, string, error) {
	if strings.HasPrefix(target, "/") {
		records, err := volume.ScanDirectoryRecordsByPath(target)
		if err != nil {
			return nil, "", err
		}
		return records, target, nil
	}

	inodeNumber, err := strconv.ParseUint(target, 10, 64)
	if err != nil {
		return nil, "", fmt.Errorf("selector must be absolute path or inode number")
	}
	records, err := volume.ScanDirectoryRecords(inodeNumber)
	if err != nil {
		return nil, "", err
	}
	return records, fmt.Sprintf("inode:%d", inodeNumber), nil
}
