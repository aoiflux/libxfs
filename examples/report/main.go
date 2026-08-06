package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"

	"github.com/aoiflux/libxfs"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> [root_path] [max_entries] [verification_mode]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./report disk.img")
		fmt.Println("  ./report disk.img /home")
		fmt.Println("  ./report disk.img / 500")
		fmt.Println("  ./report disk.img / 500 strict")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	rootPath := "/"
	if len(os.Args) >= 3 {
		rootPath = os.Args[2]
	}

	maxEntries := 0
	if len(os.Args) >= 4 {
		n, err := strconv.Atoi(os.Args[3])
		if err != nil {
			log.Fatalf("invalid max_entries value %q", os.Args[3])
		}
		maxEntries = n
	}

	verificationMode := libxfs.VerificationModeBestEffort
	if len(os.Args) >= 5 {
		verificationMode = libxfs.VerificationMode(os.Args[4])
		if verificationMode != libxfs.VerificationModeBestEffort && verificationMode != libxfs.VerificationModeStrict {
			log.Fatalf("invalid verification_mode %q (expected best_effort or strict)", os.Args[4])
		}
	}

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	report, err := volume.ReportWithOptions(libxfs.ReportOptions{
		RootPath:                  rootPath,
		MaxEntries:                maxEntries,
		IncludeDirectoryArtifacts: true,
		VerificationMode:          verificationMode,
	})
	if err != nil {
		log.Fatalf("failed to generate report: %v", err)
	}

	fmt.Fprintln(os.Stderr, report.Summary())

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		log.Fatalf("failed to encode report: %v", err)
	}
}
