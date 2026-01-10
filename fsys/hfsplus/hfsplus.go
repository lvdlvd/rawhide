// Package hfsplus implements read-only HFS+ filesystem support.
// Currently only detection and basic info are implemented.
package hfsplus

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/lvdlvd/rawhide/fsys"
)

const (
	hfsPlusSig = 0x482B // 'H+'
	hfsxSig    = 0x4858 // 'HX' (case-sensitive HFS+)
	volumeHeaderOffset = 1024
)

// FS implements a read-only HFS+ filesystem (skeleton)
type FS struct {
	r            io.ReaderAt
	size         int64
	signature    uint16
	version      uint16
	blockSize    uint32
	totalBlocks  uint32
	freeBlocks   uint32
	createDate   uint32
	modifyDate   uint32
	backupDate   uint32
	checkedDate  uint32
	fileCount    uint32
	folderCount  uint32
}

// Open opens an HFS+ filesystem from the given reader
func Open(r io.ReaderAt, size int64) (fsys.FS, error) {
	// Volume header is at offset 1024
	header := make([]byte, 512)
	if _, err := r.ReadAt(header, volumeHeaderOffset); err != nil {
		return nil, fmt.Errorf("reading HFS+ volume header: %w", err)
	}

	// Check signature (big-endian)
	sig := binary.BigEndian.Uint16(header[0:2])
	if sig != hfsPlusSig && sig != hfsxSig {
		return nil, nil // Not HFS+
	}

	f := &FS{r: r, size: size}
	f.signature = sig
	f.version = binary.BigEndian.Uint16(header[2:4])
	// attributes at 4:8
	// lastMountedVersion at 8:12
	// journalInfoBlock at 12:16
	f.createDate = binary.BigEndian.Uint32(header[16:20])
	f.modifyDate = binary.BigEndian.Uint32(header[20:24])
	f.backupDate = binary.BigEndian.Uint32(header[24:28])
	f.checkedDate = binary.BigEndian.Uint32(header[28:32])
	f.fileCount = binary.BigEndian.Uint32(header[32:36])
	f.folderCount = binary.BigEndian.Uint32(header[36:40])
	f.blockSize = binary.BigEndian.Uint32(header[40:44])
	f.totalBlocks = binary.BigEndian.Uint32(header[44:48])
	f.freeBlocks = binary.BigEndian.Uint32(header[48:52])

	return f, nil
}

func (f *FS) Type() string {
	if f.signature == hfsxSig {
		return "HFSX"
	}
	return "HFS+"
}

func (f *FS) Close() error { return nil }
func (f *FS) BaseReader() io.ReaderAt { return f.r }

// hfsTime converts HFS+ timestamp (seconds since 1904-01-01) to time.Time
func hfsTime(t uint32) time.Time {
	if t == 0 {
		return time.Time{}
	}
	// HFS+ epoch is 1904-01-01 00:00:00 UTC
	// Unix epoch is 1970-01-01 00:00:00 UTC
	// Difference is 2082844800 seconds
	const hfsEpochDiff = 2082844800
	return time.Unix(int64(t)-hfsEpochDiff, 0).UTC()
}

// Info returns filesystem information as a formatted string
func (f *FS) Info() string {
	typeName := "HFS+"
	if f.signature == hfsxSig {
		typeName = "HFSX (case-sensitive)"
	}
	
	totalSize := uint64(f.blockSize) * uint64(f.totalBlocks)
	freeSize := uint64(f.blockSize) * uint64(f.freeBlocks)
	usedSize := totalSize - freeSize
	
	info := fmt.Sprintf("%s Volume\n"+
		"  Version: %d\n"+
		"  Block size: %d bytes\n"+
		"  Total blocks: %d\n"+
		"  Free blocks: %d\n"+
		"  Total size: %d bytes (%.2f GB)\n"+
		"  Used: %d bytes (%.2f GB)\n"+
		"  Free: %d bytes (%.2f GB)\n"+
		"  Files: %d\n"+
		"  Folders: %d",
		typeName,
		f.version,
		f.blockSize,
		f.totalBlocks,
		f.freeBlocks,
		totalSize, float64(totalSize)/(1024*1024*1024),
		usedSize, float64(usedSize)/(1024*1024*1024),
		freeSize, float64(freeSize)/(1024*1024*1024),
		f.fileCount,
		f.folderCount)

	if !hfsTime(f.createDate).IsZero() {
		info += fmt.Sprintf("\n  Created: %s", hfsTime(f.createDate).Format(time.RFC3339))
	}
	if !hfsTime(f.modifyDate).IsZero() {
		info += fmt.Sprintf("\n  Modified: %s", hfsTime(f.modifyDate).Format(time.RFC3339))
	}
	
	return info
}

var errNotImplemented = fmt.Errorf("HFS+: not yet implemented")

// Open implements fs.FS
func (f *FS) Open(name string) (fs.File, error) {
	if name == "." {
		return &hfsRoot{fs: f}, nil
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
		return &hfsRootInfo{fs: f}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: errNotImplemented}
}

// hfsRoot represents the root directory
type hfsRoot struct {
	fs *FS
}

func (r *hfsRoot) Stat() (fs.FileInfo, error) { return &hfsRootInfo{fs: r.fs}, nil }
func (r *hfsRoot) Read([]byte) (int, error)   { return 0, errNotImplemented }
func (r *hfsRoot) Close() error               { return nil }

// hfsRootInfo provides FileInfo for root
type hfsRootInfo struct {
	fs *FS
}

func (i *hfsRootInfo) Name() string       { return "." }
func (i *hfsRootInfo) Size() int64        { return 0 }
func (i *hfsRootInfo) Mode() fs.FileMode  { return fs.ModeDir | 0755 }
func (i *hfsRootInfo) ModTime() time.Time { return hfsTime(i.fs.modifyDate) }
func (i *hfsRootInfo) IsDir() bool        { return true }
func (i *hfsRootInfo) Sys() any           { return nil }
