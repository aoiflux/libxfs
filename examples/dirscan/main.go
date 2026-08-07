package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/aoiflux/libxfs"
)

// dirscan demonstrates scanning a directory that may be large or damaged.
//
// It shows the two things a forensic caller has to get right: enabling
// best-effort recovery so a damaged block does not discard everything already
// found, and separating verified entries from probabilistic carve candidates.

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <directory_inode_or_path> [max_entries]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./dirscan disk.img /")
		fmt.Println("  ./dirscan disk.img /var/log")
		fmt.Println("  ./dirscan disk.img 128 5000")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	target := os.Args[2]

	maxEntries := 0
	if len(os.Args) > 3 {
		parsed, err := strconv.Atoi(os.Args[3])
		if err != nil {
			log.Fatalf("Invalid max_entries %q: %v", os.Args[3], err)
		}
		maxEntries = parsed
	}

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	inodeNumber, err := resolveTarget(volume, target)
	if err != nil {
		log.Fatalf("Failed to resolve %q: %v", target, err)
	}

	// BestEffort keeps whatever parsed before the damage and reports what went
	// wrong, instead of failing the whole scan on one malformed record.
	listing, err := volume.ScanDirectoryRecordsWithOptions(inodeNumber, libxfs.DirectoryScanOptions{
		BestEffort: true,
		MaxEntries: maxEntries,
	})
	if err != nil {
		log.Fatalf("Scan failed for %q: %v", target, err)
	}

	fmt.Printf("Directory: %s (inode %d)\n", target, inodeNumber)
	fmt.Printf("Layout:    %s across %d directory block(s)\n", listing.Format, listing.BlocksScanned)
	if listing.Truncated {
		fmt.Println("WARNING:   a safety cap was reached; these results are incomplete")
	}
	fmt.Println()

	var verified, carved, freeSlots int
	for _, record := range listing.Records {
		switch {
		case record.IsVerified():
			verified++
		case record.IsProbabilistic():
			carved++
		default:
			freeSlots++
		}
	}

	fmt.Printf("=== Active entries (%d) ===\n", verified)
	for _, record := range listing.Records {
		if !record.IsVerified() {
			continue
		}
		fmt.Printf("  %-40s inode=%-10d type=%s\n",
			record.Name, record.InodeNumber, libxfs.DirEntryFileTypeName(record.FileType))
	}

	// Carved records are candidates recovered from reclaimed space. They may be
	// stale, partially overwritten, or coincidental byte patterns, so they are
	// reported separately and never mixed in with facts.
	fmt.Printf("\n=== Deleted candidates (%d) ===\n", carved)
	if carved == 0 {
		fmt.Println("  none recovered")
	}
	for _, record := range listing.Records {
		if !record.IsProbabilistic() {
			continue
		}
		fmt.Printf("  %-40s inode=%-10d confidence=%-6s block=%d offset=%d\n",
			record.Name, record.InodeNumber, record.Confidence, record.BlockIndex, record.LogicalOffset)
		fmt.Printf("  %-40s evidence: %v\n", "", record.ConfidenceReasons)
	}

	fmt.Printf("\n=== Free slots: %d ===\n", freeSlots)

	if len(listing.Anomalies) > 0 {
		fmt.Printf("\n=== Anomalies (%d) ===\n", len(listing.Anomalies))
		for _, anomaly := range listing.Anomalies {
			fmt.Printf("  [%s] %s: %s\n", anomaly.Severity, anomaly.Code, anomaly.Message)
		}
	}
}

// resolveTarget accepts either an inode number or an absolute path.
func resolveTarget(volume *libxfs.Volume, target string) (uint64, error) {
	if inodeNumber, err := strconv.ParseUint(target, 10, 64); err == nil {
		return inodeNumber, nil
	}
	return volume.ResolveInodeByPath(target)
}
