//go:build !linux

// The oracle reads st_ino, st_nlink and the mount tree of a live XFS
// filesystem, none of which exist off Linux. This stub keeps `go build ./...`
// working on other platforms rather than failing with "build constraints
// exclude all Go files".
package main

import (
	"fmt"
	"os"
	"runtime"
)

func main() {
	fmt.Fprintf(os.Stderr,
		"oracle: ground truth must be produced on Linux from a mounted XFS filesystem (this is %s)\n",
		runtime.GOOS)
	os.Exit(1)
}
