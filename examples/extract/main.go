package main

import (
	"fmt"
	"log"
	"os"
	"path"
	"strings"

	"github.com/aoiflux/libxfs"
)

const maxSymlinkHops = 16

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		fmt.Printf("Usage: %s <xfs_volume_or_image> <xfs_file_path> [output_file]\n", os.Args[0])
		fmt.Println("\nExamples:")
		fmt.Println("  ./extract disk.img /etc/hosts")
		fmt.Println("  ./extract disk.img /etc/hosts ./hosts.txt")
		fmt.Println("  ./extract \\\\.\\C: /path/in/xfs/file.bin ./file.bin")
		os.Exit(1)
	}

	volumePath := os.Args[1]
	xfsPath := os.Args[2]

	outputPath := ""
	if len(os.Args) == 4 {
		outputPath = os.Args[3]
	} else {
		outputPath = path.Base(xfsPath)
		if outputPath == "." || outputPath == "/" || outputPath == "" {
			log.Fatalf("Cannot infer output file name from XFS path %q; provide [output_file]", xfsPath)
		}
	}

	volume, err := libxfs.OpenVolumeFromPath(volumePath)
	if err != nil {
		log.Fatalf("Failed to parse XFS volume: %v", err)
	}
	defer volume.Close()

	resolvedPath, err := resolveExtractPath(volume, xfsPath)
	if err != nil {
		log.Fatalf("Failed to resolve XFS path %q: %v", xfsPath, err)
	}

	inode, err := volume.OpenInodeByPath(resolvedPath)
	if err != nil {
		log.Fatalf("Failed to open resolved file path %q: %v", resolvedPath, err)
	}
	if (inode.FileMode & 0xf000) != libxfs.FileTypeRegularFile {
		log.Fatalf("XFS path %q resolves to non-regular file (mode=0x%04x)", resolvedPath, inode.FileMode)
	}

	data, err := volume.ReadFileDataByPath(resolvedPath)
	if err != nil {
		log.Fatalf("Failed to read file %q: %v", resolvedPath, err)
	}

	if err := os.WriteFile(outputPath, data, 0o644); err != nil {
		log.Fatalf("Failed to write output file %q: %v", outputPath, err)
	}

	fmt.Printf("Extracted %d byte(s) from %s to %s\n", len(data), resolvedPath, outputPath)
}

func resolveExtractPath(volume *libxfs.Volume, xfsPath string) (string, error) {
	current := xfsPath

	for hop := 0; hop <= maxSymlinkHops; hop++ {
		inode, err := volume.OpenInodeByPath(current)
		if err != nil {
			return "", err
		}

		fileType := inode.FileMode & 0xf000
		switch fileType {
		case libxfs.FileTypeRegularFile:
			return current, nil
		case libxfs.FileTypeDirectory:
			return "", fmt.Errorf("path %q is a directory", current)
		case libxfs.FileTypeSymbolicLink:
			linkTargetBytes, err := volume.ReadFileDataByPath(current)
			if err != nil {
				return "", fmt.Errorf("read symlink target for %q: %w", current, err)
			}
			linkTarget := strings.TrimRight(string(linkTargetBytes), "\x00")
			if linkTarget == "" {
				return "", fmt.Errorf("empty symlink target for %q", current)
			}
			if strings.HasPrefix(linkTarget, "/") {
				current = path.Clean(linkTarget)
			} else {
				current = path.Clean(path.Join(path.Dir(current), linkTarget))
			}
		default:
			return "", fmt.Errorf("path %q has unsupported file type mode=0x%04x", current, inode.FileMode)
		}
	}

	return "", fmt.Errorf("symlink resolution exceeded %d hops for %q", maxSymlinkHops, xfsPath)
}
