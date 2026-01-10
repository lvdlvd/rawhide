// Package fsys provides a read-only filesystem interface for disk images.
package fsys

import (
	"io"
	"io/fs"
)

// Range represents a byte range [Start, End) where Start is inclusive
// and End is exclusive (one past the last byte).
type Range struct {
	Start int64 // First byte of the range (inclusive)
	End   int64 // One past the last byte (exclusive)
}

// Size returns the size of the range in bytes
func (r Range) Size() int64 {
	return r.End - r.Start
}

// FS represents a read-only filesystem that can be opened from a disk image.
// It embeds io/fs.FS and adds image-specific functionality.
type FS interface {
	fs.FS
	fs.ReadDirFS
	fs.StatFS

	// Type returns the filesystem type name (e.g., "FAT32", "NTFS", "ext4")
	Type() string

	// Close releases any resources held by the filesystem
	Close() error
}

// FreeBlocker is an optional interface for filesystems that can report free space
type FreeBlocker interface {
	// FreeBlocks returns a list of free byte ranges in the filesystem image.
	// Each range is [Start, End) where Start is inclusive and End is exclusive.
	// Ranges are returned in ascending order and do not overlap.
	FreeBlocks() ([]Range, error)
}

// Opener is a function that attempts to open a filesystem from a reader.
// It returns nil, nil if the filesystem type doesn't match.
// It returns nil, error if the type matches but opening fails.
type Opener func(r io.ReaderAt, size int64) (FS, error)

// ReadOnlyError is returned for any write operation
type ReadOnlyError struct{}

func (e ReadOnlyError) Error() string {
	return "filesystem is read-only"
}

// FileInfo provides extended file information
type FileInfo interface {
	fs.FileInfo

	// Inode returns the inode number (0 for filesystems without inodes)
	Inode() uint64
}
