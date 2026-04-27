package libxfs

import "encoding/binary"

func readUint16BE(data []byte, off int) (uint16, bool) {
	if off < 0 || off+2 > len(data) {
		return 0, false
	}
	return binary.BigEndian.Uint16(data[off : off+2]), true
}

func readUint32BE(data []byte, off int) (uint32, bool) {
	if off < 0 || off+4 > len(data) {
		return 0, false
	}
	return binary.BigEndian.Uint32(data[off : off+4]), true
}

func readUint64BE(data []byte, off int) (uint64, bool) {
	if off < 0 || off+8 > len(data) {
		return 0, false
	}
	return binary.BigEndian.Uint64(data[off : off+8]), true
}

func readBytes(data []byte, off, n int) ([]byte, bool) {
	if off < 0 || n < 0 || off+n > len(data) {
		return nil, false
	}
	return data[off : off+n], true
}

func signedSecondsWithNanos(seconds uint32, nanos uint32) int64 {
	base := int64(int32(seconds)) * 1_000_000_000
	if base > 0 {
		return base + int64(nanos)
	}
	return base - int64(nanos)
}
