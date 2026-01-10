// Package fsys provides a read-only filesystem interface for disk images.
package fsys

import (
	"fmt"
	"io"
	"io/fs"
	"sort"
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

// Extent represents a mapping from logical file offset to physical image offset
type Extent struct {
	Logical  int64 // Offset within the file
	Physical int64 // Offset within the image
	Length   int64 // Length of this extent
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

// ExtentMapper is an optional interface for filesystems that can report
// the physical location of file data within the image
type ExtentMapper interface {
	// FileExtents returns the list of extents that map a file's logical
	// offsets to physical offsets in the image. Returns error if path
	// doesn't exist or is a directory.
	FileExtents(path string) ([]Extent, error)
}

// ExtentReaderAt wraps an io.ReaderAt and a list of extents to provide
// a view of a file's data without loading it entirely into memory
type ExtentReaderAt struct {
	r       io.ReaderAt
	extents []Extent
	size    int64
}

// NewExtentReaderAt creates a new ExtentReaderAt from a base reader and extents.
// If the base reader is itself an ExtentReaderAt, the extents are composed
// to create a flattened mapping directly to the underlying reader.
func NewExtentReaderAt(r io.ReaderAt, extents []Extent, size int64) *ExtentReaderAt {
	// Sort extents by logical offset
	sorted := make([]Extent, len(extents))
	copy(sorted, extents)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Logical < sorted[j].Logical
	})

	// If r is already an ExtentReaderAt, compose the mappings
	if inner, ok := r.(*ExtentReaderAt); ok {
		composed := ComposeExtents(sorted, inner.extents)
		return &ExtentReaderAt{r: inner.r, extents: composed, size: size}
	}

	return &ExtentReaderAt{r: r, extents: sorted, size: size}
}

// ComposeExtents takes outer extents (which map logical offsets to "physical"
// offsets in an inner coordinate space) and inner extents (which map that
// inner coordinate space to actual physical offsets), and returns composed
// extents that map directly from outer logical to actual physical.
//
// For example, if outer maps [0,100) -> [1000,1100) and inner maps [1000,1100) -> [5000,5100),
// the composed result maps [0,100) -> [5000,5100).
func ComposeExtents(outer, inner []Extent) []Extent {
	var composed []Extent

	for _, o := range outer {
		// o.Physical is a logical offset in the inner coordinate space
		// We need to translate [o.Physical, o.Physical+o.Length) through inner extents

		remaining := o.Length
		innerLogical := o.Physical
		outerLogical := o.Logical

		for remaining > 0 {
			// Find inner extent containing innerLogical
			found := false
			for _, i := range inner {
				iEnd := i.Logical + i.Length

				// Check if innerLogical falls within this inner extent
				if innerLogical >= i.Logical && innerLogical < iEnd {
					// Calculate how much of this inner extent we can use
					offsetInInner := innerLogical - i.Logical
					availableInInner := i.Length - offsetInInner
					useLength := remaining
					if useLength > availableInInner {
						useLength = availableInInner
					}

					// Create composed extent
					composed = append(composed, Extent{
						Logical:  outerLogical,
						Physical: i.Physical + offsetInInner,
						Length:   useLength,
					})

					outerLogical += useLength
					innerLogical += useLength
					remaining -= useLength
					found = true
					break
				}
			}

			if !found {
				// Gap in inner extents (sparse region) - skip this portion
				// Find next inner extent start
				nextStart := int64(-1)
				for _, i := range inner {
					if i.Logical > innerLogical {
						if nextStart < 0 || i.Logical < nextStart {
							nextStart = i.Logical
						}
					}
				}

				if nextStart < 0 {
					// No more inner extents, we're done with this outer extent
					break
				}

				// Skip the gap
				gap := nextStart - innerLogical
				if gap > remaining {
					gap = remaining
				}
				outerLogical += gap
				innerLogical += gap
				remaining -= gap
			}
		}
	}

	return composed
}

// Size returns the logical size of the file
func (e *ExtentReaderAt) Size() int64 {
	return e.size
}

// ReadAt implements io.ReaderAt
func (e *ExtentReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset")
	}
	if off >= e.size {
		return 0, io.EOF
	}

	// Limit read to file size
	if off+int64(len(p)) > e.size {
		p = p[:e.size-off]
	}

	totalRead := 0
	remaining := len(p)

	for remaining > 0 && off < e.size {
		// Find the extent containing this offset
		ext, found := e.findExtent(off)
		if !found {
			// Gap in extents (sparse file) - fill with zeros
			gapEnd := e.nextExtentStart(off)
			if gapEnd > e.size {
				gapEnd = e.size
			}
			zeroLen := int(gapEnd - off)
			if zeroLen > remaining {
				zeroLen = remaining
			}
			for i := 0; i < zeroLen; i++ {
				p[totalRead+i] = 0
			}
			totalRead += zeroLen
			remaining -= zeroLen
			off += int64(zeroLen)
			continue
		}

		// Calculate how much we can read from this extent
		extentOffset := off - ext.Logical
		extentRemaining := ext.Length - extentOffset
		toRead := int(extentRemaining)
		if toRead > remaining {
			toRead = remaining
		}

		// Read from the physical location
		physOffset := ext.Physical + extentOffset
		nr, err := e.r.ReadAt(p[totalRead:totalRead+toRead], physOffset)
		totalRead += nr
		remaining -= nr
		off += int64(nr)

		if err != nil && err != io.EOF {
			return totalRead, err
		}
		if nr < toRead {
			return totalRead, io.EOF
		}
	}

	if totalRead == 0 && off >= e.size {
		return 0, io.EOF
	}

	return totalRead, nil
}

// findExtent finds the extent containing the given logical offset
func (e *ExtentReaderAt) findExtent(off int64) (Extent, bool) {
	for _, ext := range e.extents {
		if off >= ext.Logical && off < ext.Logical+ext.Length {
			return ext, true
		}
	}
	return Extent{}, false
}

// nextExtentStart returns the start of the next extent after the given offset
func (e *ExtentReaderAt) nextExtentStart(off int64) int64 {
	for _, ext := range e.extents {
		if ext.Logical > off {
			return ext.Logical
		}
	}
	return e.size
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
