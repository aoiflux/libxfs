package libxfs

import "os"

// OpenVolumeFromPath opens an XFS volume from a filesystem path.
//
// This is a convenience wrapper around os.Open + Open. The returned volume
// owns the underlying file handle, and Close will close it.
func OpenVolumeFromPath(path string) (*Volume, error) {
	if path == "" {
		return nil, wrapParseError(0, "path", ErrInvalidPath)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, wrapIOError("open", 0, 0, err)
	}

	volume, err := Open(file)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	volume.sourceCloser = file
	return volume, nil
}
