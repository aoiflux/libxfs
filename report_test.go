package libxfs

import (
	"encoding/binary"
	"errors"
	"testing"
)

func buildMinimalReportDisk() []byte {
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
	copy(disk[108:120], []byte("TESTVOL"))
	disk[123] = 4
	disk[124] = 3
	binary.BigEndian.PutUint32(disk[204:208], 0)

	copy(disk[1024:1028], []byte("XAGI"))
	binary.BigEndian.PutUint32(disk[1028:1032], 1)
	binary.BigEndian.PutUint32(disk[1044:1048], 5)
	binary.BigEndian.PutUint32(disk[1048:1052], 1)
	binary.BigEndian.PutUint32(disk[1056:1060], 0)

	btreeOff := 5 * 4096
	copy(disk[btreeOff:btreeOff+4], []byte("IAB3"))
	binary.BigEndian.PutUint16(disk[btreeOff+4:btreeOff+6], 0)
	binary.BigEndian.PutUint16(disk[btreeOff+6:btreeOff+8], 1)
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

	return disk
}

func TestReportBuildsRootEntryAndSummary(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildMinimalReportDisk()})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	report, err := vol.Report()
	if err != nil {
		t.Fatalf("Report failed: %v", err)
	}
	if report.Volume.Type != "XFS" {
		t.Fatalf("unexpected volume type: %+v", report.Volume)
	}
	if len(report.Files) != 1 {
		t.Fatalf("expected one file report (root only), got %d", len(report.Files))
	}
	if report.Files[0].Path != "/" || report.Files[0].InodeNumber != 32 {
		t.Fatalf("unexpected root file report: %+v", report.Files[0])
	}
	if report.Summary() == "" {
		t.Fatalf("expected non-empty human summary")
	}
	if report.Provenance.ParserVersion != Version {
		t.Fatalf("expected parser version %q, got %q", Version, report.Provenance.ParserVersion)
	}
	if report.Provenance.VerificationMode != VerificationModeBestEffort {
		t.Fatalf("expected best-effort mode, got %q", report.Provenance.VerificationMode)
	}
}

func TestReportWithOptionsMaxEntries(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildMinimalReportDisk()})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	report, err := vol.ReportWithOptions(ReportOptions{
		RootPath:   "/",
		MaxEntries: 1,
	})
	if err != nil {
		t.Fatalf("ReportWithOptions failed: %v", err)
	}
	if len(report.Files) != 1 {
		t.Fatalf("expected report to honor max entries, got %d", len(report.Files))
	}
}

func TestReportWithOptionsStrictFailsOnSuperblockCRCMismatch(t *testing.T) {
	vol, err := Open(&mockReaderAt{data: buildMinimalReportDisk()})
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	_, err = vol.ReportWithOptions(ReportOptions{
		RootPath:         "/",
		VerificationMode: VerificationModeStrict,
	})
	if !errors.Is(err, ErrVerificationFailed) {
		t.Fatalf("expected ErrVerificationFailed in strict mode, got %v", err)
	}
}

func TestInodeForensicReportIncludesFragmentOffsets(t *testing.T) {
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

	report, err := vol.InodeForensicReport(33)
	if err != nil {
		t.Fatalf("InodeForensicReport failed: %v", err)
	}
	if len(report.Fragments) != 1 {
		t.Fatalf("expected exactly one fragment, got %d", len(report.Fragments))
	}

	fragment := report.Fragments[0]
	if fragment.StartOffset != 6*4096 {
		t.Fatalf("unexpected fragment start offset: got %d", fragment.StartOffset)
	}
	if fragment.EndOffset != 7*4096 {
		t.Fatalf("unexpected fragment end offset: got %d", fragment.EndOffset)
	}
	if fragment.LengthBytes != 4096 {
		t.Fatalf("unexpected fragment length: got %d", fragment.LengthBytes)
	}
}
