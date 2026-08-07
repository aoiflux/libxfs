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

// saturateUint32 narrows a count, clamping rather than wrapping so that an
// overflowed value can never read as a small one.
func saturateUint32(value uint64) uint32 {
	if value > 0xffffffff {
		return 0xffffffff
	}
	return uint32(value)
}

func saturateUint16(value uint32) uint16 {
	if value > 0xffff {
		return 0xffff
	}
	return uint16(value)
}

// readInodeTimestamp decodes one on-disk inode timestamp.
//
// XFS has two encodings. The legacy one is a pair of 32-bit big-endian
// seconds/nanoseconds values. The bigtime encoding, signalled by
// sb_features_incompat, is a single 64-bit count of nanoseconds since
// 1901-12-13 20:45:52 UTC. Decoding a bigtime value as a legacy pair yields a
// timestamp roughly 27 years in the past, which looks plausible enough to go
// unnoticed.
func readInodeTimestamp(data []byte, offset int, bigTime bool) int64 {
	if !bigTime {
		seconds, _ := readUint32BE(data, offset)
		nanos, _ := readUint32BE(data, offset+4)
		return signedSecondsWithNanos(seconds, nanos)
	}

	raw, ok := readUint64BE(data, offset)
	if !ok {
		return 0
	}
	return bigTimeToUnixNanos(raw)
}

// bigTimeToUnixNanos converts a bigtime counter to Unix nanoseconds.
//
// The result is clamped rather than allowed to wrap: Unix nanoseconds in an
// int64 only reach the year 2262, while bigtime itself runs to 2486.
func bigTimeToUnixNanos(raw uint64) int64 {
	seconds := int64(raw/1_000_000_000) - xfsBigTimeEpochOffsetSeconds
	nanos := int64(raw % 1_000_000_000)

	const maxSeconds = (1<<63 - 1) / 1_000_000_000
	const minSeconds = -(1 << 63) / 1_000_000_000
	if seconds > maxSeconds {
		return 1<<63 - 1
	}
	if seconds < minSeconds {
		return -(1 << 63)
	}
	return seconds*1_000_000_000 + nanos
}
