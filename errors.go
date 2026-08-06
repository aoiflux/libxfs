package libxfs

import (
	"errors"
	"fmt"
)

var (
	ErrInvalidSuperblock      = errors.New("invalid or corrupted XFS superblock")
	ErrInvalidInode           = errors.New("invalid or corrupted XFS inode")
	ErrInvalidInodeNumber     = errors.New("invalid inode number")
	ErrInvalidPath            = errors.New("invalid path")
	ErrInvalidInodeInfo       = errors.New("invalid allocation-group inode information")
	ErrInvalidAttributeData   = errors.New("invalid attribute fork data")
	ErrUnsupportedDirFormat   = errors.New("unsupported directory format")
	ErrInodeNotFound          = errors.New("inode not found")
	ErrUnsupportedFeatureFlag = errors.New("unsupported XFS feature flag")
	ErrUnsupportedXattrFormat = errors.New("unsupported extended attribute format")
	ErrVolumeClosed           = errors.New("volume is closed")
	ErrVerificationFailed     = errors.New("forensic verification failed")
)

type ParseError struct {
	Offset int64
	Field  string
	Err    error
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("parse offset=%d field=%s: %v", e.Offset, e.Field, e.Err)
}

func (e *ParseError) Unwrap() error {
	return e.Err
}

type IOError struct {
	Op     string
	Offset int64
	Size   int
	Err    error
}

func (e *IOError) Error() string {
	return fmt.Sprintf("io %s offset=%d size=%d: %v", e.Op, e.Offset, e.Size, e.Err)
}

func (e *IOError) Unwrap() error {
	return e.Err
}

type VolumeError struct {
	Op  string
	Err error
}

func (e *VolumeError) Error() string {
	return fmt.Sprintf("volume %s: %v", e.Op, e.Err)
}

func (e *VolumeError) Unwrap() error {
	return e.Err
}

func wrapParseError(offset int64, field string, err error) error {
	if err == nil {
		return nil
	}
	return &ParseError{Offset: offset, Field: field, Err: err}
}

func wrapIOError(op string, offset int64, size int, err error) error {
	if err == nil {
		return nil
	}
	return &IOError{Op: op, Offset: offset, Size: size, Err: err}
}

func wrapVolumeError(op string, err error) error {
	if err == nil {
		return nil
	}
	return &VolumeError{Op: op, Err: err}
}
