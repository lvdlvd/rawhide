// Package fat implements read-only FAT12/16/32 filesystem support.
package fat

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/luuk/fscat/fsys"
)

// FS implements a read-only FAT filesystem
type FS struct {
	r    io.ReaderAt
	size int64
	bpb  bpb
	fat  fatTable
	typ  string
}

// bpb contains the BIOS Parameter Block fields we need
type bpb struct {
	bytesPerSector    uint16
	sectorsPerCluster uint8
	reservedSectors   uint16
	numFATs           uint8
	rootEntryCount    uint16 // 0 for FAT32
	totalSectors      uint32
	fatSize           uint32 // in sectors
	rootCluster       uint32 // FAT32 only
	firstDataSector   uint32
	dataSectors       uint32
	countOfClusters   uint32
	isFAT32           bool
}

// fatTable provides access to the FAT
type fatTable struct {
	r           io.ReaderAt
	startOffset int64
	isFAT32     bool
	isFAT12     bool
}

// Open opens a FAT filesystem from the given reader
func Open(r io.ReaderAt, size int64) (fsys.FS, error) {
	header := make([]byte, 512)
	if _, err := r.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("reading boot sector: %w", err)
	}

	// Verify boot sector signature
	if header[510] != 0x55 || header[511] != 0xAA {
		return nil, nil // Not a FAT filesystem
	}

	fs := &FS{r: r, size: size}
	if err := fs.parseBPB(header); err != nil {
		return nil, err
	}

	// Set up FAT table access
	fs.fat = fatTable{
		r:           r,
		startOffset: int64(fs.bpb.reservedSectors) * int64(fs.bpb.bytesPerSector),
		isFAT32:     fs.bpb.isFAT32,
		isFAT12:     fs.bpb.countOfClusters < 4085,
	}

	return fs, nil
}

func (f *FS) parseBPB(header []byte) error {
	f.bpb.bytesPerSector = binary.LittleEndian.Uint16(header[11:13])
	f.bpb.sectorsPerCluster = header[13]
	f.bpb.reservedSectors = binary.LittleEndian.Uint16(header[14:16])
	f.bpb.numFATs = header[16]
	f.bpb.rootEntryCount = binary.LittleEndian.Uint16(header[17:19])

	totalSectors16 := binary.LittleEndian.Uint16(header[19:21])
	fatSize16 := binary.LittleEndian.Uint16(header[22:24])
	totalSectors32 := binary.LittleEndian.Uint32(header[32:36])

	if totalSectors16 != 0 {
		f.bpb.totalSectors = uint32(totalSectors16)
	} else {
		f.bpb.totalSectors = totalSectors32
	}

	if fatSize16 != 0 {
		f.bpb.fatSize = uint32(fatSize16)
		f.bpb.isFAT32 = false
	} else {
		f.bpb.fatSize = binary.LittleEndian.Uint32(header[36:40])
		f.bpb.rootCluster = binary.LittleEndian.Uint32(header[44:48])
		f.bpb.isFAT32 = true
	}

	rootDirSectors := ((uint32(f.bpb.rootEntryCount) * 32) + uint32(f.bpb.bytesPerSector) - 1) / uint32(f.bpb.bytesPerSector)
	f.bpb.firstDataSector = uint32(f.bpb.reservedSectors) + (uint32(f.bpb.numFATs) * f.bpb.fatSize) + rootDirSectors
	f.bpb.dataSectors = f.bpb.totalSectors - f.bpb.firstDataSector
	f.bpb.countOfClusters = f.bpb.dataSectors / uint32(f.bpb.sectorsPerCluster)

	// Determine type string
	// If the filesystem is structurally FAT32 (uses extended BPB), report FAT32
	// Otherwise, use cluster count to distinguish FAT12/16
	if f.bpb.isFAT32 {
		f.typ = "FAT32"
	} else if f.bpb.countOfClusters < 4085 {
		f.typ = "FAT12"
	} else {
		f.typ = "FAT16"
	}

	return nil
}

func (f *FS) Type() string  { return f.typ }
func (f *FS) Close() error  { return nil }
func (f *FS) BaseReader() io.ReaderAt { return f.r }

// FreeBlocks returns the list of free byte ranges in the FAT filesystem.
// Free clusters are those with a FAT entry value of 0.
func (f *FS) FreeBlocks() ([]fsys.Range, error) {
	var ranges []fsys.Range
	clusterSize := int64(f.clusterSize())

	var inFreeRange bool
	var rangeStart int64

	// Iterate through all data clusters (starting at cluster 2)
	for cluster := uint32(2); cluster < f.bpb.countOfClusters+2; cluster++ {
		entry, err := f.fat.next(cluster)
		if err != nil {
			return nil, fmt.Errorf("reading FAT entry %d: %w", cluster, err)
		}

		// Check if cluster is free (entry == 0)
		isFree := (entry == 0)

		offset := f.clusterToOffset(cluster)

		if isFree && !inFreeRange {
			// Start new free range
			rangeStart = offset
			inFreeRange = true
		} else if !isFree && inFreeRange {
			// End current free range
			ranges = append(ranges, fsys.Range{Start: rangeStart, End: offset})
			inFreeRange = false
		}
	}

	// Close final range if still in one
	if inFreeRange {
		lastCluster := f.bpb.countOfClusters + 2 - 1
		endOffset := f.clusterToOffset(lastCluster) + clusterSize
		ranges = append(ranges, fsys.Range{Start: rangeStart, End: endOffset})
	}

	return ranges, nil
}

// FileExtents returns the physical extents for a file
func (f *FS) FileExtents(name string) ([]fsys.Extent, error) {
	if name == "." || name == "" {
		return nil, fmt.Errorf("cannot get extents for directory")
	}

	entry, _, err := f.lookup(name)
	if err != nil {
		return nil, err
	}

	if entry.attr&attrDirectory != 0 {
		return nil, fmt.Errorf("cannot get extents for directory")
	}

	return f.clusterChainExtents(entry.cluster, int64(entry.size))
}

// clusterChainExtents returns extents for a cluster chain
func (f *FS) clusterChainExtents(startCluster uint32, fileSize int64) ([]fsys.Extent, error) {
	if startCluster < 2 {
		return nil, nil // Empty file
	}

	var extents []fsys.Extent
	clusterSize := int64(f.clusterSize())
	cluster := startCluster
	logicalOffset := int64(0)
	remaining := fileSize

	// Track current extent for coalescing contiguous clusters
	var currentExtent *fsys.Extent

	for remaining > 0 {
		physOffset := f.clusterToOffset(cluster)
		extentLen := clusterSize
		if extentLen > remaining {
			extentLen = remaining
		}

		// Try to extend current extent if contiguous
		if currentExtent != nil &&
			currentExtent.Physical+currentExtent.Length == physOffset {
			currentExtent.Length += extentLen
		} else {
			// Start new extent
			if currentExtent != nil {
				extents = append(extents, *currentExtent)
			}
			currentExtent = &fsys.Extent{
				Logical:  logicalOffset,
				Physical: physOffset,
				Length:   extentLen,
			}
		}

		logicalOffset += extentLen
		remaining -= extentLen

		if remaining <= 0 {
			break
		}

		// Get next cluster
		next, err := f.fat.next(cluster)
		if err != nil {
			return nil, fmt.Errorf("reading FAT entry for cluster %d: %w", cluster, err)
		}

		if f.fat.isEOF(next) {
			break
		}
		if next < 2 || next >= f.bpb.countOfClusters+2 {
			break
		}
		cluster = next
	}

	if currentExtent != nil {
		extents = append(extents, *currentExtent)
	}

	return extents, nil
}
func (f *FS) clusterToOffset(cluster uint32) int64 {
	return int64(f.bpb.firstDataSector)*int64(f.bpb.bytesPerSector) +
		int64(cluster-2)*int64(f.bpb.sectorsPerCluster)*int64(f.bpb.bytesPerSector)
}

// clusterSize returns the size of one cluster in bytes
func (f *FS) clusterSize() int {
	return int(f.bpb.sectorsPerCluster) * int(f.bpb.bytesPerSector)
}

// readCluster reads a single cluster
func (f *FS) readCluster(cluster uint32) ([]byte, error) {
	size := f.clusterSize()
	data := make([]byte, size)
	offset := f.clusterToOffset(cluster)
	if _, err := f.r.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return data, nil
}

// readClusterChain reads all clusters in a chain
func (f *FS) readClusterChain(startCluster uint32, maxSize int64) ([]byte, error) {
	if startCluster < 2 {
		return nil, fmt.Errorf("invalid start cluster: %d", startCluster)
	}

	var data []byte
	cluster := startCluster
	clusterSize := f.clusterSize()

	for {
		clusterData, err := f.readCluster(cluster)
		if err != nil {
			return nil, fmt.Errorf("reading cluster %d: %w", cluster, err)
		}
		data = append(data, clusterData...)

		if maxSize > 0 && int64(len(data)) >= maxSize {
			break
		}

		next, err := f.fat.next(cluster)
		if err != nil {
			return nil, fmt.Errorf("reading FAT entry for cluster %d: %w", cluster, err)
		}

		if f.fat.isEOF(next) {
			break
		}
		if next < 2 || next >= f.bpb.countOfClusters+2 {
			break
		}
		cluster = next

		// Safety limit
		if len(data) > 1<<30 {
			return nil, fmt.Errorf("cluster chain too long")
		}
	}

	if maxSize > 0 && int64(len(data)) > maxSize {
		data = data[:maxSize]
	}

	_ = clusterSize // silence unused warning
	return data, nil
}

// next returns the next cluster in the chain
func (t *fatTable) next(cluster uint32) (uint32, error) {
	if t.isFAT12 {
		return t.nextFAT12(cluster)
	} else if t.isFAT32 {
		return t.nextFAT32(cluster)
	}
	return t.nextFAT16(cluster)
}

func (t *fatTable) nextFAT12(cluster uint32) (uint32, error) {
	offset := t.startOffset + int64(cluster)*3/2
	buf := make([]byte, 2)
	if _, err := t.r.ReadAt(buf, offset); err != nil {
		return 0, err
	}
	val := binary.LittleEndian.Uint16(buf)
	if cluster%2 == 0 {
		return uint32(val & 0x0FFF), nil
	}
	return uint32(val >> 4), nil
}

func (t *fatTable) nextFAT16(cluster uint32) (uint32, error) {
	offset := t.startOffset + int64(cluster)*2
	buf := make([]byte, 2)
	if _, err := t.r.ReadAt(buf, offset); err != nil {
		return 0, err
	}
	return uint32(binary.LittleEndian.Uint16(buf)), nil
}

func (t *fatTable) nextFAT32(cluster uint32) (uint32, error) {
	offset := t.startOffset + int64(cluster)*4
	buf := make([]byte, 4)
	if _, err := t.r.ReadAt(buf, offset); err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint32(buf) & 0x0FFFFFFF, nil
}

func (t *fatTable) isEOF(cluster uint32) bool {
	if t.isFAT12 {
		return cluster >= 0x0FF8
	} else if t.isFAT32 {
		return cluster >= 0x0FFFFFF8
	}
	return cluster >= 0xFFF8
}

// dirEntry represents a FAT directory entry
type dirEntry struct {
	name      string
	ext       string
	attr      uint8
	cluster   uint32
	size      uint32
	modTime   time.Time
	isLFN     bool
	lfnParts  []string
}

const (
	attrReadOnly  = 0x01
	attrHidden    = 0x02
	attrSystem    = 0x04
	attrVolumeID  = 0x08
	attrDirectory = 0x10
	attrArchive   = 0x20
	attrLFN       = 0x0F
)

// readRootDir reads the root directory
func (f *FS) readRootDir() ([]dirEntry, error) {
	if f.bpb.isFAT32 {
		return f.readDir(f.bpb.rootCluster)
	}

	// FAT12/16: root directory is at fixed location
	rootStart := int64(f.bpb.reservedSectors)*int64(f.bpb.bytesPerSector) +
		int64(f.bpb.numFATs)*int64(f.bpb.fatSize)*int64(f.bpb.bytesPerSector)
	rootSize := int64(f.bpb.rootEntryCount) * 32

	data := make([]byte, rootSize)
	if _, err := f.r.ReadAt(data, rootStart); err != nil {
		return nil, err
	}

	return f.parseDirEntries(data)
}

// readDir reads a directory at the given cluster
func (f *FS) readDir(cluster uint32) ([]dirEntry, error) {
	data, err := f.readClusterChain(cluster, 0)
	if err != nil {
		return nil, err
	}
	return f.parseDirEntries(data)
}

func (f *FS) parseDirEntries(data []byte) ([]dirEntry, error) {
	var entries []dirEntry
	var lfnParts []string

	for i := 0; i+32 <= len(data); i += 32 {
		entry := data[i : i+32]

		// End of directory
		if entry[0] == 0x00 {
			break
		}

		// Deleted entry
		if entry[0] == 0xE5 {
			lfnParts = nil
			continue
		}

		attr := entry[11]

		// Long filename entry
		if attr == attrLFN {
			lfn := parseLFNEntry(entry)
			if entry[0]&0x40 != 0 {
				lfnParts = nil // Start of new LFN sequence
			}
			lfnParts = append([]string{lfn}, lfnParts...)
			continue
		}

		// Skip volume label
		if attr&attrVolumeID != 0 {
			lfnParts = nil
			continue
		}

		de := dirEntry{
			attr:    attr,
			size:    binary.LittleEndian.Uint32(entry[28:32]),
			cluster: uint32(binary.LittleEndian.Uint16(entry[26:28])),
		}

		if f.bpb.isFAT32 {
			de.cluster |= uint32(binary.LittleEndian.Uint16(entry[20:22])) << 16
		}

		// Parse modification time
		modTime := binary.LittleEndian.Uint16(entry[22:24])
		modDate := binary.LittleEndian.Uint16(entry[24:26])
		de.modTime = parseDOSDateTime(modDate, modTime)

		// Use LFN if available, otherwise use 8.3 name
		if len(lfnParts) > 0 {
			de.name = strings.Join(lfnParts, "")
			de.isLFN = true
		} else {
			name := strings.TrimRight(string(entry[0:8]), " ")
			ext := strings.TrimRight(string(entry[8:11]), " ")
			if entry[0] == 0x05 {
				name = "\xE5" + name[1:]
			}
			de.name = name
			de.ext = ext
			if ext != "" {
				de.name = name + "." + ext
			}
		}

		// Convert to lowercase for consistency (common for LFN-less entries)
		if !de.isLFN {
			de.name = strings.ToLower(de.name)
		}

		entries = append(entries, de)
		lfnParts = nil
	}

	return entries, nil
}

func parseLFNEntry(entry []byte) string {
	// LFN entry contains Unicode characters at specific offsets
	chars := make([]uint16, 13)
	copy(chars[0:5], []uint16{
		binary.LittleEndian.Uint16(entry[1:3]),
		binary.LittleEndian.Uint16(entry[3:5]),
		binary.LittleEndian.Uint16(entry[5:7]),
		binary.LittleEndian.Uint16(entry[7:9]),
		binary.LittleEndian.Uint16(entry[9:11]),
	})
	copy(chars[5:11], []uint16{
		binary.LittleEndian.Uint16(entry[14:16]),
		binary.LittleEndian.Uint16(entry[16:18]),
		binary.LittleEndian.Uint16(entry[18:20]),
		binary.LittleEndian.Uint16(entry[20:22]),
		binary.LittleEndian.Uint16(entry[22:24]),
		binary.LittleEndian.Uint16(entry[24:26]),
	})
	copy(chars[11:13], []uint16{
		binary.LittleEndian.Uint16(entry[28:30]),
		binary.LittleEndian.Uint16(entry[30:32]),
	})

	var result strings.Builder
	for _, c := range chars {
		if c == 0 || c == 0xFFFF {
			break
		}
		result.WriteRune(rune(c))
	}
	return result.String()
}

func parseDOSDateTime(dosDate, dosTime uint16) time.Time {
	year := int((dosDate>>9)&0x7F) + 1980
	month := time.Month((dosDate >> 5) & 0x0F)
	day := int(dosDate & 0x1F)
	hour := int((dosTime >> 11) & 0x1F)
	min := int((dosTime >> 5) & 0x3F)
	sec := int((dosTime & 0x1F) * 2)
	return time.Date(year, month, day, hour, min, sec, 0, time.UTC)
}

// fs.FS implementation

func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	if name == "." {
		return &fatDir{fs: f, name: ".", isRoot: true}, nil
	}

	entry, parent, err := f.lookup(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if entry.attr&attrDirectory != 0 {
		return &fatDir{fs: f, entry: entry, name: path.Base(name)}, nil
	}

	return &fatFile{fs: f, entry: entry, name: path.Base(name), parent: parent}, nil
}

func (f *FS) lookup(name string) (dirEntry, uint32, error) {
	parts := strings.Split(name, "/")

	var entries []dirEntry
	var err error
	var parentCluster uint32

	if f.bpb.isFAT32 {
		parentCluster = f.bpb.rootCluster
	}

	entries, err = f.readRootDir()
	if err != nil {
		return dirEntry{}, 0, err
	}

	for i, part := range parts {
		found := false
		for _, e := range entries {
			if strings.EqualFold(e.name, part) {
				if i == len(parts)-1 {
					return e, parentCluster, nil
				}
				if e.attr&attrDirectory == 0 {
					return dirEntry{}, 0, fs.ErrNotExist
				}
				parentCluster = e.cluster
				entries, err = f.readDir(e.cluster)
				if err != nil {
					return dirEntry{}, 0, err
				}
				found = true
				break
			}
		}
		if !found {
			return dirEntry{}, 0, fs.ErrNotExist
		}
	}

	return dirEntry{}, 0, fs.ErrNotExist
}

func (f *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	dir, ok := file.(fs.ReadDirFile)
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrInvalid}
	}

	return dir.ReadDir(-1)
}

func (f *FS) Stat(name string) (fs.FileInfo, error) {
	file, err := f.Open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return file.Stat()
}

// fatFile implements fs.File for regular files
type fatFile struct {
	fs      *FS
	entry   dirEntry
	name    string
	parent  uint32
	data    []byte
	offset  int64
	loaded  bool
}

func (f *fatFile) Stat() (fs.FileInfo, error) {
	return &fatFileInfo{entry: f.entry, name: f.name}, nil
}

func (f *fatFile) Read(b []byte) (int, error) {
	if !f.loaded {
		var err error
		f.data, err = f.fs.readClusterChain(f.entry.cluster, int64(f.entry.size))
		if err != nil {
			return 0, err
		}
		f.loaded = true
	}

	if f.offset >= int64(len(f.data)) {
		return 0, io.EOF
	}

	n := copy(b, f.data[f.offset:])
	f.offset += int64(n)
	return n, nil
}

func (f *fatFile) Close() error {
	f.data = nil
	return nil
}

// fatDir implements fs.File and fs.ReadDirFile for directories
type fatDir struct {
	fs     *FS
	entry  dirEntry
	name   string
	isRoot bool
	entries []fs.DirEntry
	offset int
}

func (d *fatDir) Stat() (fs.FileInfo, error) {
	if d.isRoot {
		return &fatFileInfo{name: ".", isDir: true}, nil
	}
	return &fatFileInfo{entry: d.entry, name: d.name}, nil
}

func (d *fatDir) Read(b []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *fatDir) Close() error {
	d.entries = nil
	return nil
}

func (d *fatDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.entries == nil {
		var rawEntries []dirEntry
		var err error

		if d.isRoot {
			rawEntries, err = d.fs.readRootDir()
		} else {
			rawEntries, err = d.fs.readDir(d.entry.cluster)
		}
		if err != nil {
			return nil, err
		}

		d.entries = make([]fs.DirEntry, 0, len(rawEntries))
		for _, e := range rawEntries {
			// Skip . and .. entries
			if e.name == "." || e.name == ".." {
				continue
			}
			d.entries = append(d.entries, &fatDirEntry{entry: e})
		}
	}

	if n <= 0 {
		entries := d.entries[d.offset:]
		d.offset = len(d.entries)
		return entries, nil
	}

	if d.offset >= len(d.entries) {
		return nil, io.EOF
	}

	end := d.offset + n
	if end > len(d.entries) {
		end = len(d.entries)
	}

	entries := d.entries[d.offset:end]
	d.offset = end
	return entries, nil
}

// fatDirEntry implements fs.DirEntry
type fatDirEntry struct {
	entry dirEntry
}

func (e *fatDirEntry) Name() string               { return e.entry.name }
func (e *fatDirEntry) IsDir() bool                { return e.entry.attr&attrDirectory != 0 }
func (e *fatDirEntry) Type() fs.FileMode {
	if e.IsDir() {
		return fs.ModeDir
	}
	return 0
}
func (e *fatDirEntry) Info() (fs.FileInfo, error) {
	return &fatFileInfo{entry: e.entry, name: e.entry.name}, nil
}

// fatFileInfo implements fs.FileInfo
type fatFileInfo struct {
	entry dirEntry
	name  string
	isDir bool
}

func (i *fatFileInfo) Name() string       { return i.name }
func (i *fatFileInfo) Size() int64        { return int64(i.entry.size) }
func (i *fatFileInfo) ModTime() time.Time { return i.entry.modTime }
func (i *fatFileInfo) IsDir() bool        { return i.isDir || i.entry.attr&attrDirectory != 0 }
func (i *fatFileInfo) Sys() any           { return nil }

func (i *fatFileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(0444)
	if i.IsDir() {
		mode |= fs.ModeDir | 0111
	}
	return mode
}

func init() {
	// Silence the parseDOSDateTime function - it's defined but Go can't see it's used
	_ = parseDOSDateTime
}
