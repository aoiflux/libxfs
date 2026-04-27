package libxfs

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
)

func encodeExtent(logicalBlockNumber uint64, physicalBlockNumber uint64, numberOfBlocks uint32, sparse bool) [16]byte {
	var out [16]byte
	upper := (logicalBlockNumber & 0x3fffffffffffff) << 9
	upper |= (physicalBlockNumber >> 43) & 0x1ff
	if sparse {
		upper |= uint64(1) << 63
	}

	lower := (physicalBlockNumber & 0x7ffffffffff) << 21
	lower |= uint64(numberOfBlocks & 0x1fffff)

	binary.BigEndian.PutUint64(out[0:8], upper)
	binary.BigEndian.PutUint64(out[8:16], lower)
	return out
}

type mockReaderAt struct {
	data []byte
}

func (m *mockReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 || off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestParseSuperblock(t *testing.T) {
	buf := make([]byte, 512)
	copy(buf[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(buf[4:8], 4096)
	binary.BigEndian.PutUint64(buf[8:16], 1024)
	binary.BigEndian.PutUint64(buf[48:56], 32)
	binary.BigEndian.PutUint64(buf[56:64], 32)
	binary.BigEndian.PutUint32(buf[84:88], 8)
	binary.BigEndian.PutUint32(buf[88:92], 1)
	binary.BigEndian.PutUint16(buf[100:102], 0x0015)
	binary.BigEndian.PutUint16(buf[102:104], 512)
	binary.BigEndian.PutUint16(buf[104:106], 256)
	binary.BigEndian.PutUint16(buf[106:108], 16)
	copy(buf[108:120], []byte("TESTVOL"))
	buf[123] = 4
	buf[124] = 3
	binary.BigEndian.PutUint32(buf[204:208], 0)

	sb, err := parseSuperblock(buf)
	if err != nil {
		t.Fatalf("parseSuperblock failed: %v", err)
	}
	if sb.BlockSize != 4096 {
		t.Fatalf("block size mismatch: got %d", sb.BlockSize)
	}
	if sb.DirectoryBlockSize != 4096 {
		t.Fatalf("directory block size mismatch: got %d", sb.DirectoryBlockSize)
	}
	if sb.RelativeInodeNumberBits != 7 {
		t.Fatalf("relative inode bits mismatch: got %d", sb.RelativeInodeNumberBits)
	}
}

func TestOpenAndGetRootInode(t *testing.T) {
	disk := make([]byte, 32768)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 32)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3
	binary.BigEndian.PutUint32(disk[204:208], 0)

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)
	binary.BigEndian.PutUint32(disk[1056:1060], 0)

	// inode btree root block at AG-relative block 5 (format version 5 uses IAB3)
	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	// First leaf record contains inode range [0, 64)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	inodeOff := 32 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeDirectory|0755)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 2)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 5)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 0
	disk[inodeOff+83] = 0
	copy(disk[inodeOff+176:inodeOff+181], []byte("root/"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	inode, err := vol.GetRootInode()
	if err != nil {
		t.Fatalf("GetRootInode failed: %v", err)
	}
	if !inode.IsDirectory() {
		t.Fatal("expected root inode to be directory")
	}
	if string(inode.InlineData) != "root/" {
		t.Fatalf("inline data mismatch: %q", string(inode.InlineData))
	}
}

func TestOpenRejectsInvalidSuperblock(t *testing.T) {
	_, err := Open(&mockReaderAt{data: make([]byte, 512)})
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrInvalidSuperblock) {
		t.Fatalf("expected ErrInvalidSuperblock, got %v", err)
	}
}

func TestOpenInodeNotFound(t *testing.T) {
	disk := make([]byte, 32768)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 32)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.OpenInode(96)
	if !errors.Is(err, ErrInodeNotFound) {
		t.Fatalf("expected ErrInodeNotFound, got %v", err)
	}
}

func TestOpenInodeUnusedBitmapBit(t *testing.T) {
	disk := make([]byte, 32768)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 32)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 1)

	// Mark relative inode 32 as unused in the chunk bitmap.
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], uint64(1)<<32)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.OpenInode(32)
	if !errors.Is(err, ErrInodeNotFound) {
		t.Fatalf("expected ErrInodeNotFound for unused inode, got %v", err)
	}
}

func TestReadInodeDataFromExtents(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 33)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	inodeOff := 33 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeExtents
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 8)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 1)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 0
	disk[inodeOff+83] = 0

	ext := encodeExtent(0, 6, 1, false)
	copy(disk[inodeOff+176:inodeOff+192], ext[:])

	copy(disk[6*4096:6*4096+8], []byte("helloXFS"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	buf := make([]byte, 8)
	n, err := vol.ReadInodeData(33, buf, 0)
	if err != nil {
		t.Fatalf("ReadInodeData failed: %v", err)
	}
	if n != 8 {
		t.Fatalf("ReadInodeData n=%d want=8", n)
	}
	if string(buf) != "helloXFS" {
		t.Fatalf("ReadInodeData data=%q want=%q", string(buf), "helloXFS")
	}
}

func TestReadInodeDataFromExtentsWithZeroReportedCount(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 33)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	inodeOff := 33 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeExtents
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 8)
	// Report zero extents even though one valid extent record is present.
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 0
	disk[inodeOff+83] = 0

	ext := encodeExtent(0, 6, 1, false)
	copy(disk[inodeOff+176:inodeOff+192], ext[:])

	copy(disk[6*4096:6*4096+8], []byte("helloXFS"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	buf := make([]byte, 8)
	n, err := vol.ReadInodeData(33, buf, 0)
	if err != nil {
		t.Fatalf("ReadInodeData failed: %v", err)
	}
	if n != 8 {
		t.Fatalf("ReadInodeData n=%d want=8", n)
	}
	if string(buf) != "helloXFS" {
		t.Fatalf("ReadInodeData data=%q want=%q", string(buf), "helloXFS")
	}
}

func TestAnalyzeInodeFragmentationContiguousSingleExtent(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 33)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	inodeOff := 33 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeExtents
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 8)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 1)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 0
	disk[inodeOff+83] = 0

	ext := encodeExtent(0, 6, 1, false)
	copy(disk[inodeOff+176:inodeOff+192], ext[:])

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	report, err := vol.AnalyzeInodeFragmentation(33)
	if err != nil {
		t.Fatalf("AnalyzeInodeFragmentation failed: %v", err)
	}
	if report.HasPhysicalFragmentation {
		t.Fatalf("expected no physical fragmentation, got %+v", report)
	}
	if report.HasLogicalHoles {
		t.Fatalf("expected no logical holes, got %+v", report)
	}
}

func TestAnalyzeInodeFragmentationDetectsHolesAndFragmentRuns(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 33)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	inodeOff := 33 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeExtents
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 3*4096)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 2)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 0
	disk[inodeOff+83] = 0

	first := encodeExtent(0, 6, 1, false)
	second := encodeExtent(2, 20, 1, false)
	copy(disk[inodeOff+176:inodeOff+192], first[:])
	copy(disk[inodeOff+192:inodeOff+208], second[:])

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	report, err := vol.AnalyzeInodeFragmentation(33)
	if err != nil {
		t.Fatalf("AnalyzeInodeFragmentation failed: %v", err)
	}
	if !report.HasLogicalHoles {
		t.Fatalf("expected logical holes, got %+v", report)
	}
	if !report.HasPhysicalFragmentation {
		t.Fatalf("expected physical fragmentation, got %+v", report)
	}
	if report.PhysicalFragmentRuns < 2 {
		t.Fatalf("expected at least 2 fragment runs, got %+v", report)
	}
}

func TestScanDirectoryRecordsDetectsDeletedSlotsInBlockDirectory(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 40)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	// Root directory inode 32 with one short-form entry "dir" -> inode 33.
	rootInodeOff := 32 * 256
	copy(disk[rootInodeOff:rootInodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[rootInodeOff+2:rootInodeOff+4], FileTypeDirectory|0755)
	disk[rootInodeOff+4] = 3
	disk[rootInodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[rootInodeOff+8:rootInodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[rootInodeOff+12:rootInodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[rootInodeOff+16:rootInodeOff+20], 2)
	binary.BigEndian.PutUint64(disk[rootInodeOff+56:rootInodeOff+64], 17)
	binary.BigEndian.PutUint32(disk[rootInodeOff+76:rootInodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[rootInodeOff+80:rootInodeOff+82], 0)
	disk[rootInodeOff+82] = 0
	disk[rootInodeOff+83] = 0

	rootInline := make([]byte, 0, 17)
	rootInline = append(rootInline, 1, 0)
	parentInode := make([]byte, 4)
	binary.BigEndian.PutUint32(parentInode, 32)
	rootInline = append(rootInline, parentInode...)
	rootInline = append(rootInline, 3, 0, 0, 'd', 'i', 'r', 2)
	dirInode := make([]byte, 4)
	binary.BigEndian.PutUint32(dirInode, 33)
	rootInline = append(rootInline, dirInode...)
	copy(disk[rootInodeOff+176:rootInodeOff+176+len(rootInline)], rootInline)

	// Directory inode 33 with one block-format directory data block at block 6.
	dirInodeOff := 33 * 256
	copy(disk[dirInodeOff:dirInodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[dirInodeOff+2:dirInodeOff+4], FileTypeDirectory|0755)
	disk[dirInodeOff+4] = 3
	disk[dirInodeOff+5] = ForkTypeExtents
	binary.BigEndian.PutUint32(disk[dirInodeOff+8:dirInodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[dirInodeOff+12:dirInodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[dirInodeOff+16:dirInodeOff+20], 2)
	binary.BigEndian.PutUint64(disk[dirInodeOff+56:dirInodeOff+64], 4096)
	binary.BigEndian.PutUint32(disk[dirInodeOff+76:dirInodeOff+80], 1)
	binary.BigEndian.PutUint16(disk[dirInodeOff+80:dirInodeOff+82], 0)
	disk[dirInodeOff+82] = 0
	disk[dirInodeOff+83] = 0

	ext := encodeExtent(0, 6, 1, false)
	copy(disk[dirInodeOff+176:dirInodeOff+192], ext[:])

	blockOff := 6 * 4096
	copy(disk[blockOff:blockOff+4], []byte("XD2B"))

	// Active entry at offset 16: inode=34, name="foo", type byte present (v5).
	entryOff := blockOff + 16
	binary.BigEndian.PutUint64(disk[entryOff:entryOff+8], 34)
	disk[entryOff+8] = 3
	copy(disk[entryOff+9:entryOff+12], []byte("foo"))
	disk[entryOff+12] = 1
	binary.BigEndian.PutUint16(disk[entryOff+14:entryOff+16], 16)

	// Unused/deleted region starts at offset 32 and covers up to entries_end (4080).
	unusedOff := blockOff + 32
	binary.BigEndian.PutUint16(disk[unusedOff:unusedOff+2], 0xffff)
	binary.BigEndian.PutUint16(disk[unusedOff+2:unusedOff+4], 4048)

	// Hash table (1 record) then footer with number_of_entries=1.
	footerOff := blockOff + 4096 - 8
	binary.BigEndian.PutUint32(disk[footerOff:footerOff+4], 1)
	binary.BigEndian.PutUint32(disk[footerOff+4:footerOff+8], 1)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	entries, err := vol.ListDirectoryEntries(33)
	if err != nil {
		t.Fatalf("ListDirectoryEntries failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "foo" || entries[0].InodeNumber != 34 {
		t.Fatalf("unexpected active entries: %+v", entries)
	}

	records, err := vol.ScanDirectoryRecords(33)
	if err != nil {
		t.Fatalf("ScanDirectoryRecords failed: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("unexpected number of records: %+v", records)
	}

	activeCount := 0
	deletedCount := 0
	for _, record := range records {
		if record.IsDeleted {
			deletedCount++
			if record.Offset != 32 || record.RecordLength != 4048 {
				t.Fatalf("unexpected deleted record: %+v", record)
			}
			continue
		}
		activeCount++
		if record.Name != "foo" || record.InodeNumber != 34 {
			t.Fatalf("unexpected active record: %+v", record)
		}
	}
	if activeCount != 1 || deletedCount != 1 {
		t.Fatalf("unexpected active/deleted counts in records: %+v", records)
	}
}

func TestParseBlockDirectoryRecordsCarvesDeletedCandidateWithConfidence(t *testing.T) {
	block := make([]byte, 4096)
	copy(block[0:4], []byte("XD2B"))

	// No active hash entries in tail.
	binary.BigEndian.PutUint32(block[4096-8:4096-4], 0)
	binary.BigEndian.PutUint32(block[4096-4:4096], 1)

	entriesEnd := 4096 - 8
	unusedOffset := 16
	unusedLength := entriesEnd - unusedOffset
	binary.BigEndian.PutUint16(block[unusedOffset:unusedOffset+2], 0xffff)
	binary.BigEndian.PutUint16(block[unusedOffset+2:unusedOffset+4], uint16(unusedLength))

	// Carved candidate embedded in deleted region at offset 32.
	carvedOffset := 32
	binary.BigEndian.PutUint64(block[carvedOffset:carvedOffset+8], 77)
	block[carvedOffset+8] = 4
	copy(block[carvedOffset+9:carvedOffset+13], []byte("gone"))
	block[carvedOffset+13] = 1
	binary.BigEndian.PutUint16(block[carvedOffset+14:carvedOffset+16], uint16(carvedOffset))

	records, err := parseBlockDirectoryRecords(block, 5, 0, true)
	if err != nil {
		t.Fatalf("parseBlockDirectoryRecords failed: %v", err)
	}

	foundSlot := false
	foundCarved := false
	for _, record := range records {
		if record.IsDeleted && !record.IsCarved {
			foundSlot = true
			if record.Confidence != "low" {
				t.Fatalf("expected low confidence slot record, got %+v", record)
			}
		}
		if record.IsDeleted && record.IsCarved && record.Name == "gone" {
			foundCarved = true
			if record.InodeNumber != 77 {
				t.Fatalf("unexpected carved inode number: %+v", record)
			}
			if record.Confidence != "high" {
				t.Fatalf("expected high confidence carved record, got %+v", record)
			}
		}
	}

	if !foundSlot {
		t.Fatalf("did not find deleted slot record: %+v", records)
	}
	if !foundCarved {
		t.Fatalf("did not find carved deleted record: %+v", records)
	}
}

func TestReadInodeDataFromExtentBtree(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 34)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	// inode btree for inode existence lookup
	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	// inode 34 with fork type btree
	inodeOff := 34 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeBtree
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 8)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 1)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 0
	disk[inodeOff+83] = 0

	// root node: level=1, records=1, value -> block number 6
	binary.BigEndian.PutUint16(disk[inodeOff+176:inodeOff+178], 1)
	binary.BigEndian.PutUint16(disk[inodeOff+178:inodeOff+180], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+180:inodeOff+188], 0)
	// values area starts at root+4+((dataForkSize-4)/16)*8 = root+36 for 80-byte fork data
	binary.BigEndian.PutUint64(disk[inodeOff+212:inodeOff+220], 6)

	// extent btree node block (BMA3) at relative block 6
	extentNodeOff := 6 * 4096
	copy(disk[extentNodeOff:extentNodeOff+4], []byte("BMA3"))
	binary.BigEndian.PutUint16(disk[extentNodeOff+4:extentNodeOff+6], 0)
	binary.BigEndian.PutUint16(disk[extentNodeOff+6:extentNodeOff+8], 1)

	ext := encodeExtent(0, 7, 1, false)
	copy(disk[extentNodeOff+72:extentNodeOff+88], ext[:])

	copy(disk[7*4096:7*4096+8], []byte("btreeOK!"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	buf := make([]byte, 8)
	n, err := vol.ReadInodeData(34, buf, 0)
	if err != nil {
		t.Fatalf("ReadInodeData failed: %v", err)
	}
	if n != 8 {
		t.Fatalf("ReadInodeData n=%d want=8", n)
	}
	if string(buf) != "btreeOK!" {
		t.Fatalf("ReadInodeData data=%q want=%q", string(buf), "btreeOK!")
	}
}

func TestReadInodeAttributeForkDataInline(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 35)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 35 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	// attributes fork starts 9 * 8 bytes into the data fork section
	disk[inodeOff+82] = 9
	disk[inodeOff+83] = ForkTypeInlineData

	copy(disk[inodeOff+176+72:inodeOff+176+72+6], []byte("user.a"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrData, err := vol.ReadInodeAttributeForkData(35)
	if err != nil {
		t.Fatalf("ReadInodeAttributeForkData failed: %v", err)
	}
	if len(attrData) != 8 {
		t.Fatalf("attribute fork data length=%d want=8", len(attrData))
	}
	if string(attrData[:6]) != "user.a" {
		t.Fatalf("attribute data prefix=%q want=%q", string(attrData[:6]), "user.a")
	}
}

func TestParseShortFormAttributes(t *testing.T) {
	data := make([]byte, 0, 64)
	data = append(data, 0, 0, 2, 0)

	// entry 1: user.alpha = "A"
	data = append(data, 5, 1, 0)
	data = append(data, []byte("alpha")...)
	data = append(data, 'A')

	// entry 2: trusted.beta = "BC"
	data = append(data, 4, 2, 2)
	data = append(data, []byte("beta")...)
	data = append(data, 'B', 'C')

	binary.BigEndian.PutUint16(data[0:2], uint16(len(data)))

	attrs, err := parseShortFormAttributes(data)
	if err != nil {
		t.Fatalf("parseShortFormAttributes failed: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("len(attrs)=%d want=2", len(attrs))
	}
	if attrs[0].Name != "user.alpha" || string(attrs[0].Value) != "A" {
		t.Fatalf("unexpected attr0: %+v", attrs[0])
	}
	if attrs[1].Name != "trusted.beta" || string(attrs[1].Value) != "BC" {
		t.Fatalf("unexpected attr1: %+v", attrs[1])
	}
}

func TestListInodeExtendedAttributes(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 36)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 36 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 0)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeInlineData

	attr := make([]byte, 0, 64)
	attr = append(attr, 0, 0, 1, 0)
	attr = append(attr, 3, 1, 0)
	attr = append(attr, []byte("key")...)
	attr = append(attr, 'V')
	binary.BigEndian.PutUint16(attr[0:2], uint16(len(attr)))

	copy(disk[inodeOff+176+64:inodeOff+176+64+len(attr)], attr)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(36)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("len(attrs)=%d want=1", len(attrs))
	}
	if attrs[0].Name != "user.key" || string(attrs[0].Value) != "V" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
}

func TestListInodeExtendedAttributesLeafBlock(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 37)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 37 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// attributes extent list in attributes fork
	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// leaf block at physical block 9
	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)

	// v5 leaf header starts at offset 48, size 24
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 1)

	// one leaf entry at offset 72, values offset at 120, local flag set
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	disk[leafOff+78] = 1

	// local values: value_size=1, name_size=3, name="key", value="Q"
	binary.BigEndian.PutUint16(disk[leafOff+120:leafOff+122], 1)
	disk[leafOff+122] = 3
	copy(disk[leafOff+123:leafOff+126], []byte("key"))
	disk[leafOff+126] = 'Q'

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(37)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("len(attrs)=%d want=1", len(attrs))
	}
	if attrs[0].Name != "user.key" || string(attrs[0].Value) != "Q" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
}

func TestListInodeExtendedAttributesLeafBlockRemoteValue(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 38)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 38 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// attributes extents: logical block 0 = leaf, logical block 1 = remote value data
	attrExtent := encodeExtent(0, 9, 2, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// leaf block at physical block 9
	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 1)

	// one entry at 72, values descriptor at 120, remote flag path
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	disk[leafOff+78] = 0

	// remote descriptor: value block=1, value size=5, name size=3, name="key"
	binary.BigEndian.PutUint32(disk[leafOff+120:leafOff+124], 1)
	binary.BigEndian.PutUint32(disk[leafOff+124:leafOff+128], 5)
	disk[leafOff+128] = 3
	copy(disk[leafOff+129:leafOff+132], []byte("key"))

	// remote data block at physical block 10
	copy(disk[10*4096:10*4096+5], []byte("HELLO"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(38)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("len(attrs)=%d want=1", len(attrs))
	}
	if attrs[0].Name != "user.key" || string(attrs[0].Value) != "HELLO" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
}

func TestListInodeExtendedAttributesLeafBlockRemoteValueV5Header(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 39)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 39 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 2, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 1)
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	disk[leafOff+78] = 0
	binary.BigEndian.PutUint32(disk[leafOff+120:leafOff+124], 1)
	binary.BigEndian.PutUint32(disk[leafOff+124:leafOff+128], 5)
	disk[leafOff+128] = 3
	copy(disk[leafOff+129:leafOff+132], []byte("key"))

	// optional v5 remote value block header + payload
	remoteOff := 10 * 4096
	copy(disk[remoteOff:remoteOff+4], []byte("XARM"))
	binary.BigEndian.PutUint32(disk[remoteOff+4:remoteOff+8], 48)
	binary.BigEndian.PutUint32(disk[remoteOff+8:remoteOff+12], 5)
	copy(disk[remoteOff+48:remoteOff+53], []byte("WORLD"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(39)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("len(attrs)=%d want=1", len(attrs))
	}
	if attrs[0].Name != "user.key" || string(attrs[0].Value) != "WORLD" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
}

func TestListInodeExtendedAttributesBranchToLeafRemoteValue(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 40)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 40 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// attributes extents map logical blocks 0..2 to physical blocks 9..11
	attrExtent := encodeExtent(0, 9, 3, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// branch block at logical block 0 (physical block 9)
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+48:branchOff+50], 1)
	// one entry: name hash ignored by parser, sub block number points to logical block 1
	binary.BigEndian.PutUint32(disk[branchOff+60:branchOff+64], 1)

	// leaf block at logical block 1 (physical block 10)
	leafOff := 10 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 1)
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	disk[leafOff+78] = 0
	// remote descriptor: value block=2, value size=5, name size=3, name=key
	binary.BigEndian.PutUint32(disk[leafOff+120:leafOff+124], 2)
	binary.BigEndian.PutUint32(disk[leafOff+124:leafOff+128], 5)
	disk[leafOff+128] = 3
	copy(disk[leafOff+129:leafOff+132], []byte("key"))

	// remote value at logical block 2 (physical block 11)
	copy(disk[11*4096:11*4096+5], []byte("BRNCH"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(40)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 1 {
		t.Fatalf("len(attrs)=%d want=1", len(attrs))
	}
	if attrs[0].Name != "user.key" || string(attrs[0].Value) != "BRNCH" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
}

func TestListInodeExtendedAttributesBranchPointerOutOfRange(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 41)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 41 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// Only one mapped attributes block at logical block 0.
	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// Branch points to logical block 7, which is outside the mapped extent range.
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+48:branchOff+50], 1)
	binary.BigEndian.PutUint32(disk[branchOff+60:branchOff+64], 7)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(41)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesLeafBlockRemoteValueV5HeaderBounds(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 42)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 42 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// attributes extents: leaf at logical block 0, remote value at logical block 1
	attrExtent := encodeExtent(0, 9, 2, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 1)
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	disk[leafOff+78] = 0
	binary.BigEndian.PutUint32(disk[leafOff+120:leafOff+124], 1)
	binary.BigEndian.PutUint32(disk[leafOff+124:leafOff+128], 5)
	disk[leafOff+128] = 3
	copy(disk[leafOff+129:leafOff+132], []byte("key"))

	// Malformed v5 remote value block header: offset+size exceeds block bounds.
	remoteOff := 10 * 4096
	copy(disk[remoteOff:remoteOff+4], []byte("XARM"))
	binary.BigEndian.PutUint32(disk[remoteOff+4:remoteOff+8], 4090)
	binary.BigEndian.PutUint32(disk[remoteOff+8:remoteOff+12], 16)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(42)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesLeafBlockRemoteValueTruncatedChainV4(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 43)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0014)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IABT"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 43 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// attributes extents: only leaf + a single remote data block are mapped
	attrExtent := encodeExtent(0, 9, 2, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// v4 leaf layout: fs header 12 bytes + leaf header 20 bytes, entries start at 32
	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+12:leafOff+14], 1)
	binary.BigEndian.PutUint16(disk[leafOff+36:leafOff+38], 120)
	disk[leafOff+38] = 0
	// remote descriptor: value block=1, declared value size > one block + available mapping
	binary.BigEndian.PutUint32(disk[leafOff+120:leafOff+124], 1)
	binary.BigEndian.PutUint32(disk[leafOff+124:leafOff+128], 5000)
	disk[leafOff+128] = 3
	copy(disk[leafOff+129:leafOff+132], []byte("key"))

	// one remote data block only (logical block 1)
	for i := 0; i < 4096; i++ {
		disk[10*4096+i] = 'A'
	}

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(43)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesLeafBlockValuesOffsetInsideEntriesV4(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 44)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0014)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IABT"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 44 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// v4 leaf layout: entries are in [32,40), so values_offset=36 is invalid.
	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+12:leafOff+14], 1)
	binary.BigEndian.PutUint16(disk[leafOff+36:leafOff+38], 36)
	disk[leafOff+38] = 1

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(44)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesBranchEntriesOutOfBoundsV4(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 45)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0014)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IABT"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 45 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// v4 branch layout: fs header 12 + branch header 4 => entries start at 16.
	// number_of_entries=1024 makes entries area 8192 bytes and must be rejected.
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+12:branchOff+14], 1024)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(45)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesBranchSelfReferenceRecursionDepth(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 46)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 46 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// v5 branch block with a single pointer to itself.
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+48:branchOff+50], 1)
	binary.BigEndian.PutUint32(disk[branchOff+60:branchOff+64], 0)

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(46)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesLeafMixedEntriesFailFast(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 47)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 47 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 2)

	// entry 0 is valid local xattr at values offset 120
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	disk[leafOff+78] = 1
	binary.BigEndian.PutUint16(disk[leafOff+120:leafOff+122], 1)
	disk[leafOff+122] = 3
	copy(disk[leafOff+123:leafOff+126], []byte("key"))
	disk[leafOff+126] = 'A'

	// entry 1 is corrupt: values offset points inside entries table.
	binary.BigEndian.PutUint16(disk[leafOff+84:leafOff+86], 80)
	disk[leafOff+86] = 1

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(47)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesLeafUnsupportedNamespaceFlags(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 48)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 48 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 1, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	leafOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[leafOff+8:leafOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafOff+48:leafOff+50], 1)
	binary.BigEndian.PutUint16(disk[leafOff+76:leafOff+78], 120)
	// Unsupported namespace flags: 0x08 (masked by 0x7e -> unsupported branch)
	disk[leafOff+78] = 0x09
	binary.BigEndian.PutUint16(disk[leafOff+120:leafOff+122], 1)
	disk[leafOff+122] = 3
	copy(disk[leafOff+123:leafOff+126], []byte("key"))
	disk[leafOff+126] = 'Z'

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(48)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrUnsupportedXattrFormat) {
		t.Fatalf("expected ErrUnsupportedXattrFormat, got %v", err)
	}
}

func TestListInodeExtendedAttributesBranchChildCorruptionFailFast(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 49)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 49 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// attributes extents map logical blocks 0..2 to physical blocks 9..11
	attrExtent := encodeExtent(0, 9, 3, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// branch block at logical block 0 with 2 children: block 1 (valid), block 2 (corrupt)
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+48:branchOff+50], 2)
	binary.BigEndian.PutUint32(disk[branchOff+60:branchOff+64], 1)
	binary.BigEndian.PutUint32(disk[branchOff+68:branchOff+72], 2)

	// child leaf 1: valid local xattr
	leaf1Off := 10 * 4096
	binary.BigEndian.PutUint16(disk[leaf1Off+8:leaf1Off+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leaf1Off+48:leaf1Off+50], 1)
	binary.BigEndian.PutUint16(disk[leaf1Off+76:leaf1Off+78], 120)
	disk[leaf1Off+78] = 1
	binary.BigEndian.PutUint16(disk[leaf1Off+120:leaf1Off+122], 1)
	disk[leaf1Off+122] = 3
	copy(disk[leaf1Off+123:leaf1Off+126], []byte("key"))
	disk[leaf1Off+126] = 'A'

	// child leaf 2: corrupt values_offset points into entries area
	leaf2Off := 11 * 4096
	binary.BigEndian.PutUint16(disk[leaf2Off+8:leaf2Off+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leaf2Off+48:leaf2Off+50], 1)
	binary.BigEndian.PutUint16(disk[leaf2Off+76:leaf2Off+78], 72)
	disk[leaf2Off+78] = 1

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ListInodeExtendedAttributes(49)
	if err == nil {
		t.Fatal("expected ListInodeExtendedAttributes error")
	}
	if !errors.Is(err, ErrInvalidAttributeData) {
		t.Fatalf("expected ErrInvalidAttributeData, got %v", err)
	}
}

func TestListInodeExtendedAttributesBranchChildOrderingDeterministic(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 50)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 50 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	// logical blocks 0..2 map to physical blocks 9..11
	attrExtent := encodeExtent(0, 9, 3, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// branch at logical block 0 with explicit child order: first 2, then 1
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+48:branchOff+50], 2)
	binary.BigEndian.PutUint32(disk[branchOff+60:branchOff+64], 2)
	binary.BigEndian.PutUint32(disk[branchOff+68:branchOff+72], 1)

	// leaf at logical block 2 (physical 11): user.second=S
	leafSecondOff := 11 * 4096
	binary.BigEndian.PutUint16(disk[leafSecondOff+8:leafSecondOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafSecondOff+48:leafSecondOff+50], 1)
	binary.BigEndian.PutUint16(disk[leafSecondOff+76:leafSecondOff+78], 120)
	disk[leafSecondOff+78] = 1
	binary.BigEndian.PutUint16(disk[leafSecondOff+120:leafSecondOff+122], 1)
	disk[leafSecondOff+122] = 6
	copy(disk[leafSecondOff+123:leafSecondOff+129], []byte("second"))
	disk[leafSecondOff+129] = 'S'

	// leaf at logical block 1 (physical 10): user.first=F
	leafFirstOff := 10 * 4096
	binary.BigEndian.PutUint16(disk[leafFirstOff+8:leafFirstOff+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leafFirstOff+48:leafFirstOff+50], 1)
	binary.BigEndian.PutUint16(disk[leafFirstOff+76:leafFirstOff+78], 120)
	disk[leafFirstOff+78] = 1
	binary.BigEndian.PutUint16(disk[leafFirstOff+120:leafFirstOff+122], 1)
	disk[leafFirstOff+122] = 5
	copy(disk[leafFirstOff+123:leafFirstOff+128], []byte("first"))
	disk[leafFirstOff+128] = 'F'

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(50)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("len(attrs)=%d want=2", len(attrs))
	}
	if attrs[0].Name != "user.second" || string(attrs[0].Value) != "S" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
	if attrs[1].Name != "user.first" || string(attrs[1].Value) != "F" {
		t.Fatalf("unexpected attrs[1]: %+v", attrs[1])
	}
}

func TestListInodeExtendedAttributesDuplicateNamesPreservedOrder(t *testing.T) {
	disk := make([]byte, 131072)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 51)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	inodeBtreeOff := 5 * 4096
	copy(disk[inodeBtreeOff:inodeBtreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+4:inodeBtreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[inodeBtreeOff+6:inodeBtreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+56:inodeBtreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[inodeBtreeOff+60:inodeBtreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[inodeBtreeOff+64:inodeBtreeOff+72], 0)

	inodeOff := 51 * 256
	copy(disk[inodeOff:inodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[inodeOff+2:inodeOff+4], FileTypeRegularFile|0644)
	disk[inodeOff+4] = 3
	disk[inodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[inodeOff+8:inodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+12:inodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[inodeOff+16:inodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[inodeOff+56:inodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[inodeOff+76:inodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[inodeOff+80:inodeOff+82], 1)
	disk[inodeOff+82] = 8
	disk[inodeOff+83] = ForkTypeExtents

	attrExtent := encodeExtent(0, 9, 3, false)
	copy(disk[inodeOff+176+64:inodeOff+176+80], attrExtent[:])

	// branch at logical block 0 with child order: 1 then 2
	branchOff := 9 * 4096
	binary.BigEndian.PutUint16(disk[branchOff+8:branchOff+10], 0x3ebe)
	binary.BigEndian.PutUint16(disk[branchOff+48:branchOff+50], 2)
	binary.BigEndian.PutUint32(disk[branchOff+60:branchOff+64], 1)
	binary.BigEndian.PutUint32(disk[branchOff+68:branchOff+72], 2)

	// leaf at logical block 1 (physical 10): user.dup=1
	leaf1Off := 10 * 4096
	binary.BigEndian.PutUint16(disk[leaf1Off+8:leaf1Off+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leaf1Off+48:leaf1Off+50], 1)
	binary.BigEndian.PutUint16(disk[leaf1Off+76:leaf1Off+78], 120)
	disk[leaf1Off+78] = 1
	binary.BigEndian.PutUint16(disk[leaf1Off+120:leaf1Off+122], 1)
	disk[leaf1Off+122] = 3
	copy(disk[leaf1Off+123:leaf1Off+126], []byte("dup"))
	disk[leaf1Off+126] = '1'

	// leaf at logical block 2 (physical 11): user.dup=2
	leaf2Off := 11 * 4096
	binary.BigEndian.PutUint16(disk[leaf2Off+8:leaf2Off+10], 0x3bee)
	binary.BigEndian.PutUint16(disk[leaf2Off+48:leaf2Off+50], 1)
	binary.BigEndian.PutUint16(disk[leaf2Off+76:leaf2Off+78], 120)
	disk[leaf2Off+78] = 1
	binary.BigEndian.PutUint16(disk[leaf2Off+120:leaf2Off+122], 1)
	disk[leaf2Off+122] = 3
	copy(disk[leaf2Off+123:leaf2Off+126], []byte("dup"))
	disk[leaf2Off+126] = '2'

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	attrs, err := vol.ListInodeExtendedAttributes(51)
	if err != nil {
		t.Fatalf("ListInodeExtendedAttributes failed: %v", err)
	}
	if len(attrs) != 2 {
		t.Fatalf("len(attrs)=%d want=2", len(attrs))
	}
	if attrs[0].Name != "user.dup" || string(attrs[0].Value) != "1" {
		t.Fatalf("unexpected attrs[0]: %+v", attrs[0])
	}
	if attrs[1].Name != "user.dup" || string(attrs[1].Value) != "2" {
		t.Fatalf("unexpected attrs[1]: %+v", attrs[1])
	}
}

func TestFriendlyPathAndReadConvenienceAPIs(t *testing.T) {
	disk := make([]byte, 65536)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 32)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	// Root directory inode at 32 with one short-form entry "a" -> inode 33.
	rootInodeOff := 32 * 256
	copy(disk[rootInodeOff:rootInodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[rootInodeOff+2:rootInodeOff+4], FileTypeDirectory|0755)
	disk[rootInodeOff+4] = 3
	disk[rootInodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[rootInodeOff+8:rootInodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[rootInodeOff+12:rootInodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[rootInodeOff+16:rootInodeOff+20], 2)
	binary.BigEndian.PutUint64(disk[rootInodeOff+56:rootInodeOff+64], 15)
	binary.BigEndian.PutUint32(disk[rootInodeOff+76:rootInodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[rootInodeOff+80:rootInodeOff+82], 0)
	disk[rootInodeOff+82] = 0
	disk[rootInodeOff+83] = 0

	rootInline := make([]byte, 0, 15)
	rootInline = append(rootInline, 1, 0)
	parentInodeBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(parentInodeBytes, 32)
	rootInline = append(rootInline, parentInodeBytes...)
	rootInline = append(rootInline, 1, 0, 0, 'a', 1)
	inodeBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(inodeBytes, 33)
	rootInline = append(rootInline, inodeBytes...)
	copy(disk[rootInodeOff+176:rootInodeOff+176+len(rootInline)], rootInline)

	// Regular file inode at 33 with inline data "hello".
	fileInodeOff := 33 * 256
	copy(disk[fileInodeOff:fileInodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[fileInodeOff+2:fileInodeOff+4], FileTypeRegularFile|0644)
	disk[fileInodeOff+4] = 3
	disk[fileInodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[fileInodeOff+8:fileInodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[fileInodeOff+12:fileInodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[fileInodeOff+16:fileInodeOff+20], 1)
	binary.BigEndian.PutUint64(disk[fileInodeOff+56:fileInodeOff+64], 5)
	binary.BigEndian.PutUint32(disk[fileInodeOff+76:fileInodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[fileInodeOff+80:fileInodeOff+82], 0)
	disk[fileInodeOff+82] = 0
	disk[fileInodeOff+83] = 0
	copy(disk[fileInodeOff+176:fileInodeOff+181], []byte("hello"))

	vol, err := Open(&mockReaderAt{data: disk})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	entries, err := vol.ListDirectoryEntriesByPath("/")
	if err != nil {
		t.Fatalf("ListDirectoryEntriesByPath failed: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "a" || entries[0].InodeNumber != 33 {
		t.Fatalf("unexpected directory entries: %+v", entries)
	}

	inode, err := vol.OpenInodeByPath("/a")
	if err != nil {
		t.Fatalf("OpenInodeByPath failed: %v", err)
	}
	if inode.IsDirectory() {
		t.Fatal("expected /a to be a file")
	}

	data, err := vol.ReadFileDataByPath("/a")
	if err != nil {
		t.Fatalf("ReadFileDataByPath failed: %v", err)
	}
	if string(data) != "hello" {
		t.Fatalf("unexpected file data: %q", string(data))
	}
}

func TestOpenVolumeFromPath(t *testing.T) {
	disk := make([]byte, 32768)

	copy(disk[0:4], []byte("XFSB"))
	binary.BigEndian.PutUint32(disk[4:8], 4096)
	binary.BigEndian.PutUint64(disk[8:16], 1024)
	binary.BigEndian.PutUint64(disk[48:56], 32)
	binary.BigEndian.PutUint64(disk[56:64], 32)
	binary.BigEndian.PutUint32(disk[84:88], 8)
	binary.BigEndian.PutUint32(disk[88:92], 1)
	binary.BigEndian.PutUint16(disk[100:102], 0x0015)
	binary.BigEndian.PutUint16(disk[102:104], 512)
	binary.BigEndian.PutUint16(disk[104:106], 256)
	binary.BigEndian.PutUint16(disk[106:108], 16)
	disk[123] = 4
	disk[124] = 3

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
	binary.BigEndian.PutUint32(disk[btreeOff+56:btreeOff+60], 0)
	binary.BigEndian.PutUint32(disk[btreeOff+60:btreeOff+64], 0)
	binary.BigEndian.PutUint64(disk[btreeOff+64:btreeOff+72], 0)

	rootInodeOff := 32 * 256
	copy(disk[rootInodeOff:rootInodeOff+2], []byte("IN"))
	binary.BigEndian.PutUint16(disk[rootInodeOff+2:rootInodeOff+4], FileTypeDirectory|0755)
	disk[rootInodeOff+4] = 3
	disk[rootInodeOff+5] = ForkTypeInlineData
	binary.BigEndian.PutUint32(disk[rootInodeOff+8:rootInodeOff+12], 1000)
	binary.BigEndian.PutUint32(disk[rootInodeOff+12:rootInodeOff+16], 1000)
	binary.BigEndian.PutUint32(disk[rootInodeOff+16:rootInodeOff+20], 2)
	binary.BigEndian.PutUint64(disk[rootInodeOff+56:rootInodeOff+64], 0)
	binary.BigEndian.PutUint32(disk[rootInodeOff+76:rootInodeOff+80], 0)
	binary.BigEndian.PutUint16(disk[rootInodeOff+80:rootInodeOff+82], 0)
	disk[rootInodeOff+82] = 0
	disk[rootInodeOff+83] = 0

	tmpDir := t.TempDir()
	imagePath := tmpDir + string(os.PathSeparator) + "xfs.img"
	if err := os.WriteFile(imagePath, disk, 0o600); err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	vol, err := OpenVolumeFromPath(imagePath)
	if err != nil {
		t.Fatalf("OpenVolumeFromPath failed: %v", err)
	}
	if _, err := vol.GetRootInode(); err != nil {
		t.Fatalf("GetRootInode failed: %v", err)
	}
	if err := vol.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}
}

func TestOpenVolumeFromPathRejectsInvalidPath(t *testing.T) {
	_, err := OpenVolumeFromPath("")
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("expected ErrInvalidPath, got %v", err)
	}
}
