// Package libxfs provides a pure-Go parser for XFS metadata structures.
//
// Extended attribute listing preserves on-disk traversal order for block-based
// attribute trees, and duplicate fully-qualified names are returned as-is.
package libxfs

// Version information
const (
	// Version is the current library version
	Version = "0.1.0"

	// Author information
	Author = "libxfs contributors"
)
