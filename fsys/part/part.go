// Package part provides partition table parsing and filesystem access.
// It treats partition tables (MBR, GPT) as a quasi-filesystem where
// partitions appear as directories that can be traversed to access
// the underlying filesystems.
package part

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/luuk/fscat/detect"
	"github.com/luuk/fscat/fsys"
	"github.com/luuk/fscat/fsys/ext"
	"github.com/luuk/fscat/fsys/fat"
	"github.com/luuk/fscat/fsys/ntfs"
)

// Partition represents a single partition entry
type Partition struct {
	Index      int    // Partition index (0-based)
	Name       string // Display name (e.g., "p0", "p1")
	Type       byte   // MBR partition type or 0 for GPT
	TypeGUID   [16]byte // GPT type GUID
	StartLBA   uint64
	SizeLBA    uint64
	Bootable   bool
	Label      string // GPT partition label (if available)
	FSType     detect.Type // Detected filesystem type (lazy)
	fsTypeSet  bool
}

// SizeBytes returns the partition size in bytes
func (p *Partition) SizeBytes() int64 {
	return int64(p.SizeLBA) * 512
}

// StartOffset returns the starting byte offset
func (p *Partition) StartOffset() int64 {
	return int64(p.StartLBA) * 512
}

// FS implements fsys.FS for partition tables
type FS struct {
	r          io.ReaderAt
	size       int64
	tableType  detect.Type // MBR or GPT
	partitions []*Partition
}

// Open opens a partition table from a reader
func Open(r io.ReaderAt, size int64, tableType detect.Type) (*FS, error) {
	pfs := &FS{
		r:         r,
		size:      size,
		tableType: tableType,
	}

	var err error
	switch tableType {
	case detect.MBR:
		err = pfs.parseMBR()
	case detect.GPT:
		err = pfs.parseGPT()
	default:
		return nil, fmt.Errorf("unsupported partition table type: %s", tableType)
	}

	if err != nil {
		return nil, err
	}

	return pfs, nil
}

// parseMBR parses the MBR partition table
func (pfs *FS) parseMBR() error {
	header := make([]byte, 512)
	if _, err := pfs.r.ReadAt(header, 0); err != nil {
		return fmt.Errorf("reading MBR: %w", err)
	}

	// Verify signature
	if header[510] != 0x55 || header[511] != 0xAA {
		return fmt.Errorf("invalid MBR signature")
	}

	// Parse 4 partition entries starting at offset 446
	for i := 0; i < 4; i++ {
		entry := header[446+i*16 : 446+(i+1)*16]

		bootFlag := entry[0]
		partType := entry[4]
		lbaStart := binary.LittleEndian.Uint32(entry[8:12])
		lbaSize := binary.LittleEndian.Uint32(entry[12:16])

		// Skip empty partitions
		if partType == 0 || lbaSize == 0 {
			continue
		}

		p := &Partition{
			Index:    i,
			Name:     fmt.Sprintf("p%d", i),
			Type:     partType,
			StartLBA: uint64(lbaStart),
			SizeLBA:  uint64(lbaSize),
			Bootable: bootFlag == 0x80,
		}

		pfs.partitions = append(pfs.partitions, p)
	}

	return nil
}

// parseGPT parses the GPT partition table
func (pfs *FS) parseGPT() error {
	// GPT header is at LBA 1 (offset 512)
	header := make([]byte, 512)
	if _, err := pfs.r.ReadAt(header, 512); err != nil {
		return fmt.Errorf("reading GPT header: %w", err)
	}

	// Verify signature
	if string(header[0:8]) != "EFI PART" {
		return fmt.Errorf("invalid GPT signature")
	}

	// Parse header fields
	// headerSize := binary.LittleEndian.Uint32(header[12:16])
	partitionEntryLBA := binary.LittleEndian.Uint64(header[72:80])
	numPartitions := binary.LittleEndian.Uint32(header[80:84])
	partitionEntrySize := binary.LittleEndian.Uint32(header[84:88])

	if partitionEntrySize < 128 {
		partitionEntrySize = 128
	}

	// Limit to reasonable number
	if numPartitions > 128 {
		numPartitions = 128
	}

	// Read partition entries
	entryOffset := int64(partitionEntryLBA) * 512
	entryBuf := make([]byte, partitionEntrySize)

	for i := uint32(0); i < numPartitions; i++ {
		offset := entryOffset + int64(i)*int64(partitionEntrySize)
		if _, err := pfs.r.ReadAt(entryBuf, offset); err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("reading GPT entry %d: %w", i, err)
		}

		// Check if entry is used (type GUID not zero)
		var typeGUID [16]byte
		copy(typeGUID[:], entryBuf[0:16])
		if isZeroGUID(typeGUID) {
			continue
		}

		startLBA := binary.LittleEndian.Uint64(entryBuf[32:40])
		endLBA := binary.LittleEndian.Uint64(entryBuf[40:48])
		// attributes := binary.LittleEndian.Uint64(entryBuf[48:56])

		// Parse partition name (UTF-16LE, 36 chars max)
		nameBytes := entryBuf[56:128]
		label := parseUTF16LEString(nameBytes)

		p := &Partition{
			Index:    int(i),
			Name:     fmt.Sprintf("p%d", i),
			TypeGUID: typeGUID,
			StartLBA: startLBA,
			SizeLBA:  endLBA - startLBA + 1,
			Label:    label,
		}

		pfs.partitions = append(pfs.partitions, p)
	}

	return nil
}

func isZeroGUID(guid [16]byte) bool {
	for _, b := range guid {
		if b != 0 {
			return false
		}
	}
	return true
}

func parseUTF16LEString(data []byte) string {
	// Convert UTF-16LE to string
	u16 := make([]uint16, len(data)/2)
	for i := 0; i < len(u16); i++ {
		u16[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	// Find null terminator
	for i, v := range u16 {
		if v == 0 {
			u16 = u16[:i]
			break
		}
	}
	return string(utf16.Decode(u16))
}

// Type returns the partition table type
func (pfs *FS) Type() string {
	return pfs.tableType.String()
}

// Close releases resources
func (pfs *FS) Close() error {
	return nil
}

// Partitions returns the list of partitions
func (pfs *FS) Partitions() []*Partition {
	return pfs.partitions
}

// Open implements fs.FS
func (pfs *FS) Open(name string) (fs.File, error) {
	name = cleanPath(name)

	// Root directory
	if name == "." || name == "" {
		return &rootDir{pfs: pfs}, nil
	}

	// Parse path to find partition and subpath
	parts := strings.SplitN(name, "/", 2)
	partName := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Find partition
	part := pfs.findPartition(partName)
	if part == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	// If no subpath, return partition as directory
	if subPath == "" {
		return &partitionDir{pfs: pfs, part: part}, nil
	}

	// Open the filesystem within the partition
	innerFS, err := pfs.openPartitionFS(part)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	defer innerFS.Close()

	return innerFS.Open(subPath)
}

// ReadDir implements fs.ReadDirFS
func (pfs *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	name = cleanPath(name)

	// Root directory - list partitions
	if name == "." || name == "" {
		entries := make([]fs.DirEntry, 0, len(pfs.partitions))
		for _, p := range pfs.partitions {
			entries = append(entries, &partitionEntry{part: p, pfs: pfs})
		}
		return entries, nil
	}

	// Parse path
	parts := strings.SplitN(name, "/", 2)
	partName := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Find partition
	part := pfs.findPartition(partName)
	if part == nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}

	// Open the filesystem within the partition
	innerFS, err := pfs.openPartitionFS(part)
	if err != nil {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: err}
	}
	defer innerFS.Close()

	if subPath == "" {
		subPath = "."
	}
	return innerFS.ReadDir(subPath)
}

// Stat implements fs.StatFS
func (pfs *FS) Stat(name string) (fs.FileInfo, error) {
	name = cleanPath(name)

	// Root directory
	if name == "." || name == "" {
		return &rootInfo{pfs: pfs}, nil
	}

	// Parse path
	parts := strings.SplitN(name, "/", 2)
	partName := parts[0]
	subPath := ""
	if len(parts) > 1 {
		subPath = parts[1]
	}

	// Find partition
	part := pfs.findPartition(partName)
	if part == nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}

	// Partition info
	if subPath == "" {
		return &partitionInfo{part: part, pfs: pfs}, nil
	}

	// Delegate to inner filesystem
	innerFS, err := pfs.openPartitionFS(part)
	if err != nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: err}
	}
	defer innerFS.Close()

	return innerFS.Stat(subPath)
}

func (pfs *FS) findPartition(name string) *Partition {
	for _, p := range pfs.partitions {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// openPartitionFS opens the filesystem contained within a partition
func (pfs *FS) openPartitionFS(part *Partition) (fsys.FS, error) {
	// Create a sub-reader for the partition
	subReader := &offsetReader{
		r:      pfs.r,
		offset: part.StartOffset(),
		size:   part.SizeBytes(),
	}

	// Detect filesystem type
	fsType, err := detect.Detect(subReader)
	if err != nil {
		return nil, fmt.Errorf("detecting filesystem in partition %s: %w", part.Name, err)
	}

	if fsType == detect.Unknown {
		return nil, fmt.Errorf("unknown filesystem in partition %s", part.Name)
	}

	// Don't allow nested partition tables for now
	if fsType.IsPartitionTable() {
		return nil, fmt.Errorf("nested partition tables not supported")
	}

	// Open the appropriate filesystem
	switch {
	case fsType.IsFAT():
		return fat.Open(subReader, part.SizeBytes())
	case fsType.IsExt():
		return ext.Open(subReader, part.SizeBytes())
	case fsType == detect.NTFS:
		return ntfs.Open(subReader, part.SizeBytes())
	default:
		return nil, fmt.Errorf("unsupported filesystem type: %s", fsType)
	}
}

// DetectPartitionFS detects and returns the filesystem type for a partition
func (pfs *FS) DetectPartitionFS(part *Partition) (detect.Type, error) {
	if part.fsTypeSet {
		return part.FSType, nil
	}

	subReader := &offsetReader{
		r:      pfs.r,
		offset: part.StartOffset(),
		size:   part.SizeBytes(),
	}

	fsType, err := detect.Detect(subReader)
	if err != nil {
		return detect.Unknown, err
	}

	part.FSType = fsType
	part.fsTypeSet = true
	return fsType, nil
}

// offsetReader provides a view into a portion of an io.ReaderAt
type offsetReader struct {
	r      io.ReaderAt
	offset int64
	size   int64
}

func (r *offsetReader) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 || off >= r.size {
		return 0, io.EOF
	}
	if off+int64(len(p)) > r.size {
		p = p[:r.size-off]
	}
	return r.r.ReadAt(p, r.offset+off)
}

func cleanPath(name string) string {
	name = path.Clean(name)
	name = strings.TrimPrefix(name, "/")
	if name == "" {
		name = "."
	}
	return name
}

// rootDir represents the root directory (partition list)
type rootDir struct {
	pfs    *FS
	offset int
}

func (d *rootDir) Read(p []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: ".", Err: fmt.Errorf("is a directory")}
}

func (d *rootDir) Close() error {
	return nil
}

func (d *rootDir) Stat() (fs.FileInfo, error) {
	return &rootInfo{pfs: d.pfs}, nil
}

func (d *rootDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.offset >= len(d.pfs.partitions) {
		if n <= 0 {
			return nil, nil
		}
		return nil, io.EOF
	}

	if n <= 0 {
		n = len(d.pfs.partitions) - d.offset
	}

	end := d.offset + n
	if end > len(d.pfs.partitions) {
		end = len(d.pfs.partitions)
	}

	entries := make([]fs.DirEntry, 0, end-d.offset)
	for i := d.offset; i < end; i++ {
		entries = append(entries, &partitionEntry{part: d.pfs.partitions[i], pfs: d.pfs})
	}
	d.offset = end
	return entries, nil
}

// rootInfo provides FileInfo for the root directory
type rootInfo struct {
	pfs *FS
}

func (i *rootInfo) Name() string       { return "." }
func (i *rootInfo) Size() int64        { return 0 }
func (i *rootInfo) Mode() fs.FileMode  { return fs.ModeDir | 0755 }
func (i *rootInfo) ModTime() time.Time { return time.Time{} }
func (i *rootInfo) IsDir() bool        { return true }
func (i *rootInfo) Sys() any           { return nil }

// partitionEntry represents a partition as a directory entry
type partitionEntry struct {
	part *Partition
	pfs  *FS
}

func (e *partitionEntry) Name() string               { return e.part.Name }
func (e *partitionEntry) IsDir() bool                { return true }
func (e *partitionEntry) Type() fs.FileMode          { return fs.ModeDir }
func (e *partitionEntry) Info() (fs.FileInfo, error) { return &partitionInfo{part: e.part, pfs: e.pfs}, nil }

// partitionInfo provides FileInfo for a partition
type partitionInfo struct {
	part *Partition
	pfs  *FS
}

func (i *partitionInfo) Name() string       { return i.part.Name }
func (i *partitionInfo) Size() int64        { return i.part.SizeBytes() }
func (i *partitionInfo) Mode() fs.FileMode  { return fs.ModeDir | 0755 }
func (i *partitionInfo) ModTime() time.Time { return time.Time{} }
func (i *partitionInfo) IsDir() bool        { return true }
func (i *partitionInfo) Sys() any           { return i.part }
func (i *partitionInfo) Inode() uint64      { return uint64(i.part.Index) }

// partitionDir represents an open partition directory
type partitionDir struct {
	pfs    *FS
	part   *Partition
	offset int
	inner  fsys.FS
	done   bool
}

func (d *partitionDir) Read(p []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.part.Name, Err: fmt.Errorf("is a directory")}
}

func (d *partitionDir) Close() error {
	if d.inner != nil {
		return d.inner.Close()
	}
	return nil
}

func (d *partitionDir) Stat() (fs.FileInfo, error) {
	return &partitionInfo{part: d.part, pfs: d.pfs}, nil
}

func (d *partitionDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.inner == nil && !d.done {
		inner, err := d.pfs.openPartitionFS(d.part)
		if err != nil {
			d.done = true
			return nil, err
		}
		d.inner = inner
	}

	if d.inner == nil {
		return nil, io.EOF
	}

	return d.inner.ReadDir(".")
}

// PartitionTypeString returns a human-readable partition type
func PartitionTypeString(p *Partition) string {
	if p.Type != 0 {
		// MBR type
		switch p.Type {
		case 0x01:
			return "FAT12"
		case 0x04, 0x06, 0x0E:
			return "FAT16"
		case 0x0B, 0x0C:
			return "FAT32"
		case 0x07:
			return "NTFS/exFAT"
		case 0x05, 0x0F:
			return "Extended"
		case 0x82:
			return "Linux swap"
		case 0x83:
			return "Linux"
		case 0x8E:
			return "Linux LVM"
		case 0xEE:
			return "GPT Protective"
		case 0xEF:
			return "EFI System"
		default:
			return fmt.Sprintf("0x%02X", p.Type)
		}
	}

	// GPT GUID
	guid := p.TypeGUID
	guidStr := formatGUID(guid)

	// Check common GUIDs
	switch guidStr {
	case "C12A7328-F81F-11D2-BA4B-00A0C93EC93B":
		return "EFI System"
	case "EBD0A0A2-B9E5-4433-87C0-68B6B72699C7":
		return "Basic Data"
	case "0FC63DAF-8483-4772-8E79-3D69D8477DE4":
		return "Linux Filesystem"
	case "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F":
		return "Linux Swap"
	case "E6D6D379-F507-44C2-A23C-238F2A3DF928":
		return "Linux LVM"
	case "A19D880F-05FC-4D3B-A006-743F0F84911E":
		return "Linux RAID"
	default:
		return guidStr
	}
}

func formatGUID(guid [16]byte) string {
	// GUIDs are stored in mixed-endian format
	return fmt.Sprintf("%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X",
		binary.LittleEndian.Uint32(guid[0:4]),
		binary.LittleEndian.Uint16(guid[4:6]),
		binary.LittleEndian.Uint16(guid[6:8]),
		guid[8], guid[9],
		guid[10], guid[11], guid[12], guid[13], guid[14], guid[15])
}
