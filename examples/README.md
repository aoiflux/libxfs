# libxfs Examples

This directory contains runnable examples for libxfs, modeled after the libntfs
examples layout.

## Examples

### 1. basic - Volume Information and Root Inode Summary

Opens an XFS volume/image, prints superblock fields, shows root inode metadata,
and attempts to list root inode extended attributes.

```bash
# Linux
cd examples/basic
go run . /dev/sda1

# Disk image
go run . ../../disk.img
```

### 2. traverse - Recursive Traversal From a Selected Folder

Traverses the file system starting at a selected folder (inode number or
absolute path). This currently supports short-form inline directory decoding.

```bash
cd examples/traverse
go run . ../../disk.img /
go run . ../../disk.img 128
go run . ../../disk.img /home 4
```

### 3. xattrs - List Extended Attributes for an Inode

Lists decoded extended attributes (short-form and block-based) for one inode.

```bash
cd examples/xattrs
go run . ../../disk.img 128
```

### 4. inode_read - Read Data From a Specific Inode (Utility)

Opens a specific inode and reads data from offset 0 (default 1024 bytes).

```bash
cd examples/inode_read
go run . ../../disk.img 128
go run . ../../disk.img 128 4096
```

### 5. extract - Extract a File by XFS Path

Reads file content from an absolute path inside the XFS volume and writes it to
an output file on the host.

```bash
cd examples/extract
go run . ../../disk.img /etc/hosts
go run . ../../disk.img /etc/hosts ./hosts.txt
```

### 6. fragmentation - Analyze File Fragmentation

Shows fragmentation metrics for a file inode or absolute path, including logical
holes and physical fragment runs.

```bash
cd examples/fragmentation
go run . ../../disk.img 128
go run . ../../disk.img /carvey/go.mod
```

### 7. forensics - Scan Active and Deleted Directory Records

Scans a directory inode/path and prints active entries, deleted slots, and
carved deleted name candidates with confidence levels.

```bash
cd examples/forensics
go run . ../../disk.img 128
go run . ../../disk.img /carvey
```

## Building Examples

```bash
cd examples/basic
go build

cd ../traverse
go build

cd ../xattrs
go build

cd ../inode_read
go build

cd ../extract
go build

cd ../fragmentation
go build

cd ../forensics
go build
```

## Important: Windows Raw Drive Access

On Windows, accessing raw drives requires:

1. Administrator privileges
2. Correct path format (for example: \\.\C:)

Common errors:

- Access is denied: run terminal as Administrator
- The system cannot find the file specified: use \\.\X: format, not X: or X:\
- Failed to parse XFS volume: ensure target is actually XFS
