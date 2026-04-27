package main

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"

	"github.com/aoiflux/libxfs"
)

func main() {
	if len(os.Args) < 3 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <file_inode_or_path>\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./fragmentation disk.img 128")
		fmt.Println("  ./fragmentation disk.img /carvey/go.mod")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	target := os.Args[2]

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	report, targetDisplay, err := analyzeTarget(volume, target)
	if err != nil {
		log.Fatalf("Fragmentation analysis failed for %q: %v", target, err)
	}

	fmt.Println("=== Fragmentation Report ===")
	fmt.Printf("Target: %s\n", targetDisplay)
	fmt.Printf("Inode: %d\n", report.InodeNumber)
	fmt.Printf("Size: %d bytes\n", report.Size)
	fmt.Printf("Data extents: %d\n", report.DataExtentCount)
	fmt.Printf("Allocated extents: %d\n", report.AllocatedExtentCount)
	fmt.Printf("Sparse extents: %d\n", report.SparseExtentCount)
	fmt.Printf("Physical fragment runs: %d\n", report.PhysicalFragmentRuns)
	fmt.Printf("Has logical holes: %v\n", report.HasLogicalHoles)
	fmt.Printf("Has physical fragmentation: %v\n", report.HasPhysicalFragmentation)
	fmt.Printf("Has any fragmentation or holes: %v\n", report.HasAnyFragmentationOrHoles)
}

func analyzeTarget(volume *libxfs.Volume, target string) (libxfs.FragmentationReport, string, error) {
	if strings.HasPrefix(target, "/") {
		report, err := volume.AnalyzeInodeFragmentationByPath(target)
		if err != nil {
			return libxfs.FragmentationReport{}, "", err
		}
		return report, target, nil
	}

	inodeNumber, err := strconv.ParseUint(target, 10, 64)
	if err != nil {
		return libxfs.FragmentationReport{}, "", fmt.Errorf("selector must be absolute path or inode number")
	}
	report, err := volume.AnalyzeInodeFragmentation(inodeNumber)
	if err != nil {
		return libxfs.FragmentationReport{}, "", err
	}
	return report, fmt.Sprintf("inode:%d", inodeNumber), nil
}
