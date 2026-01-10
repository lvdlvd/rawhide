// Package part provides partition table parsing.
// It treats partition tables (MBR, GPT) as a filesystem where
// partitions appear as files that can be read or recursed into.
package part

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/luuk/fscat/detect"
	"github.com/luuk/fscat/fsys"
)

// Partition represents a single partition entry
type Partition struct {
	Index    int      // Partition index (0-based)
	Name     string   // Display name (e.g., "p0", "p1")
	Type     byte     // MBR partition type or 0 for GPT
	TypeGUID [16]byte // GPT type GUID
	StartLBA uint64
	SizeLBA  uint64
	Bootable bool
	Label    string // GPT partition label (if available)
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
		return nil, fmt.Errorf("unknown partition table type: %v", tableType)
	}

	if err != nil {
		return nil, err
	}

	return pfs, nil
}

// parseMBR parses an MBR partition table
func (pfs *FS) parseMBR() error {
	header := make([]byte, 512)
	if _, err := pfs.r.ReadAt(header, 0); err != nil {
		return fmt.Errorf("reading MBR: %w", err)
	}

	// Check signature
	if header[510] != 0x55 || header[511] != 0xAA {
		return fmt.Errorf("invalid MBR signature")
	}

	// Parse 4 partition entries at offset 446
	for i := 0; i < 4; i++ {
		entry := header[446+i*16 : 446+(i+1)*16]

		partType := entry[4]
		if partType == 0 {
			continue // Empty entry
		}

		lbaStart := binary.LittleEndian.Uint32(entry[8:12])
		lbaSize := binary.LittleEndian.Uint32(entry[12:16])

		if lbaStart == 0 || lbaSize == 0 {
			continue
		}

		pfs.partitions = append(pfs.partitions, &Partition{
			Index:    len(pfs.partitions),
			Name:     fmt.Sprintf("p%d", len(pfs.partitions)),
			Type:     partType,
			StartLBA: uint64(lbaStart),
			SizeLBA:  uint64(lbaSize),
			Bootable: entry[0] == 0x80,
		})
	}

	return nil
}

// parseGPT parses a GPT partition table
func (pfs *FS) parseGPT() error {
	// GPT header is at LBA 1 (offset 512)
	header := make([]byte, 512)
	if _, err := pfs.r.ReadAt(header, 512); err != nil {
		return fmt.Errorf("reading GPT header: %w", err)
	}

	// Check signature
	if string(header[0:8]) != "EFI PART" {
		return fmt.Errorf("invalid GPT signature")
	}

	// Parse header fields
	partitionEntryLBA := binary.LittleEndian.Uint64(header[72:80])
	numPartitionEntries := binary.LittleEndian.Uint32(header[80:84])
	partitionEntrySize := binary.LittleEndian.Uint32(header[84:88])

	if partitionEntrySize < 128 {
		return fmt.Errorf("invalid partition entry size: %d", partitionEntrySize)
	}

	// Read partition entries
	entryOffset := int64(partitionEntryLBA) * 512
	for i := uint32(0); i < numPartitionEntries; i++ {
		entry := make([]byte, partitionEntrySize)
		if _, err := pfs.r.ReadAt(entry, entryOffset+int64(i)*int64(partitionEntrySize)); err != nil {
			break
		}

		// Check if entry is used (type GUID not all zeros)
		var typeGUID [16]byte
		copy(typeGUID[:], entry[0:16])
		if isZeroGUID(typeGUID) {
			continue
		}

		startLBA := binary.LittleEndian.Uint64(entry[32:40])
		endLBA := binary.LittleEndian.Uint64(entry[40:48])

		// Parse partition name (UTF-16LE, up to 72 bytes = 36 chars)
		name := decodeUTF16LE(entry[56:128])

		pfs.partitions = append(pfs.partitions, &Partition{
			Index:    len(pfs.partitions),
			Name:     fmt.Sprintf("p%d", len(pfs.partitions)),
			TypeGUID: typeGUID,
			StartLBA: startLBA,
			SizeLBA:  endLBA - startLBA + 1,
			Label:    name,
		})
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

func decodeUTF16LE(data []byte) string {
	if len(data)%2 != 0 {
		data = data[:len(data)-1]
	}

	u16s := make([]uint16, len(data)/2)
	for i := 0; i < len(u16s); i++ {
		u16s[i] = binary.LittleEndian.Uint16(data[i*2 : i*2+2])
	}

	// Find null terminator
	for i, v := range u16s {
		if v == 0 {
			u16s = u16s[:i]
			break
		}
	}

	return string(utf16.Decode(u16s))
}

// Type returns the partition table type
func (pfs *FS) Type() string {
	return pfs.tableType.String()
}

// Close releases resources
func (pfs *FS) Close() error {
	return nil
}

// BaseReader returns the underlying ReaderAt
func (pfs *FS) BaseReader() io.ReaderAt {
	return pfs.r
}

// Info returns partition table information
func (pfs *FS) Info() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Partitions: %d\n\n", len(pfs.partitions)))
	sb.WriteString(fmt.Sprintf("%-6s %-19s %12s %12s %s\n",
		"NAME", "TYPE", "START", "SIZE", "LABEL"))

	for _, p := range pfs.partitions {
		typeStr := PartitionTypeString(p)
		label := p.Label
		if label == "" && p.Bootable {
			label = "(bootable)"
		}
		sb.WriteString(fmt.Sprintf("%-6s %-19s %12d %12s %s\n",
			p.Name,
			truncate(typeStr, 19),
			p.StartLBA,
			formatSize(p.SizeBytes()),
			label))
	}

	return sb.String()
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-1] + "…"
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1fT", float64(bytes)/TB)
	case bytes >= GB:
		return fmt.Sprintf("%.1fG", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fM", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fK", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}

// FreeBlocks returns the list of free byte ranges (gaps between partitions)
func (pfs *FS) FreeBlocks() ([]fsys.Range, error) {
	// Sort partitions by start (they should be, but ensure it)
	type partRange struct {
		start int64
		end   int64
	}
	var usedRanges []partRange

	for _, p := range pfs.partitions {
		usedRanges = append(usedRanges, partRange{
			start: p.StartOffset(),
			end:   p.StartOffset() + p.SizeBytes(),
		})
	}

	// Sort by start
	for i := 0; i < len(usedRanges); i++ {
		for j := i + 1; j < len(usedRanges); j++ {
			if usedRanges[j].start < usedRanges[i].start {
				usedRanges[i], usedRanges[j] = usedRanges[j], usedRanges[i]
			}
		}
	}

	var freeRanges []fsys.Range

	// Reserved area at start
	var reservedEnd int64
	if pfs.tableType == detect.MBR {
		reservedEnd = 512 // Just the MBR
	} else {
		reservedEnd = 34 * 512 // GPT header + entries
	}

	// Find gaps
	currentPos := reservedEnd
	for _, r := range usedRanges {
		if r.start > currentPos {
			freeRanges = append(freeRanges, fsys.Range{Start: currentPos, End: r.start})
		}
		if r.end > currentPos {
			currentPos = r.end
		}
	}

	// Space after last partition
	if currentPos < pfs.size {
		endLimit := pfs.size
		if pfs.tableType == detect.GPT {
			endLimit = pfs.size - 33*512 // Backup GPT
		}
		if currentPos < endLimit {
			freeRanges = append(freeRanges, fsys.Range{Start: currentPos, End: endLimit})
		}
	}

	return freeRanges, nil
}

// FileExtents returns the physical extents for a partition
func (pfs *FS) FileExtents(name string) ([]fsys.Extent, error) {
	name = cleanPath(name)

	if name == "." || name == "" {
		return nil, fmt.Errorf("cannot get extents for root")
	}

	part := pfs.findPartition(name)
	if part == nil {
		return nil, fmt.Errorf("partition not found: %s", name)
	}

	return []fsys.Extent{{
		Logical:  0,
		Physical: part.StartOffset(),
		Length:   part.SizeBytes(),
	}}, nil
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

	// Find partition
	part := pfs.findPartition(name)
	if part == nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}

	// Return partition as a file
	return &partitionFile{pfs: pfs, part: part}, nil
}

// ReadDir implements fs.ReadDirFS
func (pfs *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	name = cleanPath(name)

	// Root directory - list partitions
	if name == "." || name == "" {
		entries := make([]fs.DirEntry, 0, len(pfs.partitions))
		for _, p := range pfs.partitions {
			entries = append(entries, &partitionEntry{part: p})
		}
		return entries, nil
	}

	// Partitions are files, not directories
	return nil, &fs.PathError{Op: "readdir", Path: name, Err: fmt.Errorf("not a directory")}
}

// Stat implements fs.StatFS
func (pfs *FS) Stat(name string) (fs.FileInfo, error) {
	name = cleanPath(name)

	// Root directory
	if name == "." || name == "" {
		return &rootInfo{pfs: pfs}, nil
	}

	// Find partition
	part := pfs.findPartition(name)
	if part == nil {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}

	return &partitionInfo{part: part}, nil
}

func (pfs *FS) findPartition(name string) *Partition {
	for _, p := range pfs.partitions {
		if p.Name == name {
			return p
		}
	}
	return nil
}

func cleanPath(name string) string {
	name = strings.TrimPrefix(name, "/")
	name = strings.TrimSuffix(name, "/")
	if name == "" {
		return "."
	}
	return name
}

// rootDir represents the root directory
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
		entries = append(entries, &partitionEntry{part: d.pfs.partitions[i]})
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

// partitionEntry represents a partition as a directory entry (file)
type partitionEntry struct {
	part *Partition
}

func (e *partitionEntry) Name() string               { return e.part.Name }
func (e *partitionEntry) IsDir() bool                { return false }
func (e *partitionEntry) Type() fs.FileMode          { return 0 }
func (e *partitionEntry) Info() (fs.FileInfo, error) { return &partitionInfo{part: e.part}, nil }

// partitionInfo provides FileInfo for a partition
type partitionInfo struct {
	part *Partition
}

func (i *partitionInfo) Name() string       { return i.part.Name }
func (i *partitionInfo) Size() int64        { return i.part.SizeBytes() }
func (i *partitionInfo) Mode() fs.FileMode  { return 0444 }
func (i *partitionInfo) ModTime() time.Time { return time.Time{} }
func (i *partitionInfo) IsDir() bool        { return false }
func (i *partitionInfo) Sys() any           { return i.part }
func (i *partitionInfo) Inode() uint64      { return uint64(i.part.Index) }

// partitionFile represents an open partition as a file
type partitionFile struct {
	pfs    *FS
	part   *Partition
	offset int64
}

func (f *partitionFile) Stat() (fs.FileInfo, error) {
	return &partitionInfo{part: f.part}, nil
}

func (f *partitionFile) Read(p []byte) (int, error) {
	if f.offset >= f.part.SizeBytes() {
		return 0, io.EOF
	}

	toRead := int64(len(p))
	if f.offset+toRead > f.part.SizeBytes() {
		toRead = f.part.SizeBytes() - f.offset
	}

	n, err := f.pfs.r.ReadAt(p[:toRead], f.part.StartOffset()+f.offset)
	f.offset += int64(n)
	return n, err
}

func (f *partitionFile) Close() error {
	return nil
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
	// Apple partition types
	case "7C3457EF-0000-11AA-AA11-00306543ECAC":
		return "Apple APFS"
	case "48465300-0000-11AA-AA11-00306543ECAC":
		return "Apple HFS+"
	case "55465300-0000-11AA-AA11-00306543ECAC":
		return "Apple UFS"
	case "52414944-0000-11AA-AA11-00306543ECAC":
		return "Apple RAID"
	case "426F6F74-0000-11AA-AA11-00306543ECAC":
		return "Apple Boot"
	case "4C616265-6C00-11AA-AA11-00306543ECAC":
		return "Apple Label"
	case "5265636F-7665-11AA-AA11-00306543ECAC":
		return "Apple Recovery"
	case "53746F72-6167-11AA-AA11-00306543ECAC":
		return "Apple Core Storage"
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
