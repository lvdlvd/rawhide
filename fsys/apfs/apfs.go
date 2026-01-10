// Package apfs implements read-only APFS filesystem support.
// Currently only detection and basic info are implemented.
package apfs

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/luuk/fscat/fsys"
)

const (
	nxsbMagic = 0x4253584E // "NXSB" little-endian
)

// FS implements a read-only APFS filesystem (skeleton)
type FS struct {
	r         io.ReaderAt
	size      int64
	blockSize uint32
	blockCount uint64
	uuid      [16]byte
}

// containerSuperblock represents the APFS container superblock (nx_superblock_t)
type containerSuperblock struct {
	// Object header (obj_phys_t) - 32 bytes
	checksum  uint64
	oid       uint64
	xid       uint64
	objType   uint32
	objFlags  uint32

	// Container superblock fields
	magic       uint32
	blockSize   uint32
	blockCount  uint64
	features    uint64
	roCompatFeatures uint64
	incompatFeatures uint64
	uuid        [16]byte
}

// Open opens an APFS filesystem from the given reader
func Open(r io.ReaderAt, size int64) (fsys.FS, error) {
	// APFS container superblock starts at offset 0
	header := make([]byte, 128)
	if _, err := r.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("reading APFS superblock: %w", err)
	}

	// Check magic at offset 32 (after object header)
	magic := binary.LittleEndian.Uint32(header[32:36])
	if magic != nxsbMagic {
		return nil, nil // Not APFS
	}

	f := &FS{r: r, size: size}
	f.blockSize = binary.LittleEndian.Uint32(header[36:40])
	f.blockCount = binary.LittleEndian.Uint64(header[40:48])
	copy(f.uuid[:], header[72:88])

	return f, nil
}

func (f *FS) Type() string { return "APFS" }
func (f *FS) Close() error { return nil }
func (f *FS) BaseReader() io.ReaderAt { return f.r }

// BlockSize returns the container block size
func (f *FS) BlockSize() uint32 { return f.blockSize }

// BlockCount returns the total number of blocks
func (f *FS) BlockCount() uint64 { return f.blockCount }

// UUID returns the container UUID
func (f *FS) UUID() [16]byte { return f.uuid }

// Info returns filesystem information as a formatted string
func (f *FS) Info() string {
	uuid := f.uuid
	return fmt.Sprintf("APFS Container\n"+
		"  Block size: %d bytes\n"+
		"  Block count: %d\n"+
		"  Container size: %d bytes (%.2f GB)\n"+
		"  UUID: %08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		f.blockSize,
		f.blockCount,
		uint64(f.blockSize)*f.blockCount,
		float64(uint64(f.blockSize)*f.blockCount)/(1024*1024*1024),
		binary.BigEndian.Uint32(uuid[0:4]),
		binary.BigEndian.Uint16(uuid[4:6]),
		binary.BigEndian.Uint16(uuid[6:8]),
		uuid[8], uuid[9],
		uuid[10], uuid[11], uuid[12], uuid[13], uuid[14], uuid[15])
}

var errNotImplemented = fmt.Errorf("APFS: not yet implemented")

// Open implements fs.FS
func (f *FS) Open(name string) (fs.File, error) {
	if name == "." {
		return &apfsRoot{fs: f}, nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: errNotImplemented}
}

// ReadDir implements fs.ReadDirFS
func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: errNotImplemented}
}

// Stat implements fs.StatFS
func (f *FS) Stat(name string) (fs.FileInfo, error) {
	if name == "." {
		return &apfsRootInfo{fs: f}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: errNotImplemented}
}

// apfsRoot represents the root directory
type apfsRoot struct {
	fs *FS
}

func (r *apfsRoot) Stat() (fs.FileInfo, error) { return &apfsRootInfo{fs: r.fs}, nil }
func (r *apfsRoot) Read([]byte) (int, error)   { return 0, errNotImplemented }
func (r *apfsRoot) Close() error               { return nil }

// apfsRootInfo provides FileInfo for root
type apfsRootInfo struct {
	fs *FS
}

func (i *apfsRootInfo) Name() string       { return "." }
func (i *apfsRootInfo) Size() int64        { return 0 }
func (i *apfsRootInfo) Mode() fs.FileMode  { return fs.ModeDir | 0755 }
func (i *apfsRootInfo) ModTime() time.Time { return time.Time{} }
func (i *apfsRootInfo) IsDir() bool        { return true }
func (i *apfsRootInfo) Sys() any           { return nil }
