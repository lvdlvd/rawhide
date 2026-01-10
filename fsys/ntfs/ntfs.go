// Package ntfs implements read-only NTFS filesystem support.
package ntfs

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/luuk/fscat/fsys"
)

const (
	ntfsMagic = "NTFS    "

	// MFT record flags
	mftFlagInUse     = 0x01
	mftFlagDirectory = 0x02

	// Attribute types
	attrStandardInfo    = 0x10
	attrAttributeList   = 0x20
	attrFileName        = 0x30
	attrObjectID        = 0x40
	attrSecurityDesc    = 0x50
	attrVolumeName      = 0x60
	attrVolumeInfo      = 0x70
	attrData            = 0x80
	attrIndexRoot       = 0x90
	attrIndexAllocation = 0xA0
	attrBitmap          = 0xB0
	attrReparsePoint    = 0xC0
	attrEnd             = 0xFFFFFFFF

	// File name types
	fileNamePOSIX = 0
	fileNameWin32 = 1
	fileNameDOS   = 2
	fileNameBoth  = 3

	// Special MFT entries
	mftRecordMFT     = 0
	mftRecordMFTMirr = 1
	mftRecordLogFile = 2
	mftRecordVolume  = 3
	mftRecordAttrDef = 4
	mftRecordRoot    = 5
	mftRecordBitmap  = 6
	mftRecordBoot    = 7
	mftRecordBadClus = 8
	mftRecordSecure  = 9
	mftRecordUpCase  = 10
	mftRecordExtend  = 11
)

// FS implements a read-only NTFS filesystem
type FS struct {
	r               io.ReaderAt
	size            int64
	bytesPerSector  uint16
	sectorsPerCluster uint8
	mftCluster      uint64
	mftRecordSize   int32
	indexRecordSize int32
	clusterSize     int
	mftData         []byte
	mftLoaded       bool
}

// Open opens an NTFS filesystem from the given reader
func Open(r io.ReaderAt, size int64) (fsys.FS, error) {
	header := make([]byte, 512)
	if _, err := r.ReadAt(header, 0); err != nil {
		return nil, fmt.Errorf("reading boot sector: %w", err)
	}

	// Check NTFS signature
	if !bytes.Equal(header[3:11], []byte(ntfsMagic)) {
		return nil, nil // Not NTFS
	}

	fs := &FS{r: r, size: size}
	if err := fs.parseBootSector(header); err != nil {
		return nil, err
	}

	return fs, nil
}

func (f *FS) parseBootSector(header []byte) error {
	f.bytesPerSector = binary.LittleEndian.Uint16(header[0x0B:0x0D])
	f.sectorsPerCluster = header[0x0D]
	f.mftCluster = binary.LittleEndian.Uint64(header[0x30:0x38])

	// MFT record size
	mftRecordSizeByte := int8(header[0x40])
	if mftRecordSizeByte > 0 {
		f.mftRecordSize = int32(mftRecordSizeByte) * int32(f.sectorsPerCluster) * int32(f.bytesPerSector)
	} else {
		f.mftRecordSize = 1 << uint(-mftRecordSizeByte)
	}

	// Index record size
	indexRecordSizeByte := int8(header[0x44])
	if indexRecordSizeByte > 0 {
		f.indexRecordSize = int32(indexRecordSizeByte) * int32(f.sectorsPerCluster) * int32(f.bytesPerSector)
	} else {
		f.indexRecordSize = 1 << uint(-indexRecordSizeByte)
	}

	f.clusterSize = int(f.sectorsPerCluster) * int(f.bytesPerSector)

	return nil
}

func (f *FS) Type() string { return "NTFS" }
func (f *FS) Close() error { return nil }

func (f *FS) clusterOffset(cluster uint64) int64 {
	return int64(cluster) * int64(f.clusterSize)
}

func (f *FS) readCluster(cluster uint64) ([]byte, error) {
	data := make([]byte, f.clusterSize)
	offset := f.clusterOffset(cluster)
	if _, err := f.r.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return data, nil
}

// mftRecord represents an MFT record
type mftRecord struct {
	signature     [4]byte
	usaOffset     uint16
	usaCount      uint16
	lsn           uint64
	sequenceNum   uint16
	linkCount     uint16
	attrOffset    uint16
	flags         uint16
	usedSize      uint32
	allocatedSize uint32
	baseRecord    uint64
	nextAttrID    uint16
	recordNumber  uint32
	data          []byte
}

// attribute represents an NTFS attribute
type attribute struct {
	attrType     uint32
	length       uint32
	nonResident  bool
	nameLength   uint8
	nameOffset   uint16
	flags        uint16
	attrID       uint16
	name         string
	// Resident attribute
	valueLength uint32
	valueOffset uint16
	value       []byte
	// Non-resident attribute
	startVCN       uint64
	endVCN         uint64
	dataRunsOffset uint16
	allocatedSize  uint64
	realSize       uint64
	initSize       uint64
	dataRuns       []dataRun
}

type dataRun struct {
	length uint64
	offset int64 // Can be negative (sparse)
	sparse bool
}

func (f *FS) readMFTRecord(recordNum uint64) (*mftRecord, error) {
	// For record 0, read directly from mftCluster
	if recordNum == 0 || !f.mftLoaded {
		offset := f.clusterOffset(f.mftCluster) + int64(recordNum)*int64(f.mftRecordSize)
		data := make([]byte, f.mftRecordSize)
		if _, err := f.r.ReadAt(data, offset); err != nil {
			return nil, err
		}
		return f.parseMFTRecord(data, recordNum)
	}

	// For other records, use MFT data
	offset := int64(recordNum) * int64(f.mftRecordSize)
	if offset+int64(f.mftRecordSize) > int64(len(f.mftData)) {
		return nil, fmt.Errorf("MFT record %d out of range", recordNum)
	}
	return f.parseMFTRecord(f.mftData[offset:offset+int64(f.mftRecordSize)], recordNum)
}

func (f *FS) parseMFTRecord(data []byte, recordNum uint64) (*mftRecord, error) {
	if len(data) < 42 {
		return nil, fmt.Errorf("MFT record too small")
	}

	rec := &mftRecord{
		usaOffset:     binary.LittleEndian.Uint16(data[4:6]),
		usaCount:      binary.LittleEndian.Uint16(data[6:8]),
		lsn:           binary.LittleEndian.Uint64(data[8:16]),
		sequenceNum:   binary.LittleEndian.Uint16(data[16:18]),
		linkCount:     binary.LittleEndian.Uint16(data[18:20]),
		attrOffset:    binary.LittleEndian.Uint16(data[20:22]),
		flags:         binary.LittleEndian.Uint16(data[22:24]),
		usedSize:      binary.LittleEndian.Uint32(data[24:28]),
		allocatedSize: binary.LittleEndian.Uint32(data[28:32]),
		baseRecord:    binary.LittleEndian.Uint64(data[32:40]),
		nextAttrID:    binary.LittleEndian.Uint16(data[40:42]),
		recordNumber:  uint32(recordNum),
	}
	copy(rec.signature[:], data[0:4])

	// Check signature
	if string(rec.signature[:]) != "FILE" {
		return nil, fmt.Errorf("invalid MFT record signature: %q", rec.signature)
	}

	// Apply fixup array
	rec.data = make([]byte, len(data))
	copy(rec.data, data)

	if err := f.applyFixup(rec.data, rec.usaOffset, rec.usaCount); err != nil {
		return nil, err
	}

	return rec, nil
}

func (f *FS) applyFixup(data []byte, usaOffset, usaCount uint16) error {
	if usaCount < 2 {
		return nil
	}

	usaEnd := uint16(usaOffset) + uint16(usaCount)*2
	if int(usaEnd) > len(data) {
		return fmt.Errorf("fixup array out of bounds")
	}

	updateSeq := binary.LittleEndian.Uint16(data[usaOffset : usaOffset+2])
	sectorSize := 512

	for i := uint16(1); i < usaCount; i++ {
		offset := int(i) * sectorSize - 2
		if offset+2 > len(data) {
			break
		}
		expected := binary.LittleEndian.Uint16(data[offset : offset+2])
		if expected != updateSeq {
			return fmt.Errorf("fixup mismatch at offset %d", offset)
		}
		replacement := data[usaOffset+i*2 : usaOffset+i*2+2]
		copy(data[offset:offset+2], replacement)
	}

	return nil
}

func (f *FS) parseAttributes(rec *mftRecord) ([]attribute, error) {
	var attrs []attribute
	offset := int(rec.attrOffset)

	for offset+4 <= len(rec.data) {
		attrType := binary.LittleEndian.Uint32(rec.data[offset : offset+4])
		if attrType == attrEnd {
			break
		}

		if offset+16 > len(rec.data) {
			break
		}

		length := binary.LittleEndian.Uint32(rec.data[offset+4 : offset+8])
		if length == 0 || int(length) > len(rec.data)-offset {
			break
		}

		attr := attribute{
			attrType:   attrType,
			length:     length,
			nonResident: rec.data[offset+8] != 0,
			nameLength: rec.data[offset+9],
			nameOffset: binary.LittleEndian.Uint16(rec.data[offset+10 : offset+12]),
			flags:      binary.LittleEndian.Uint16(rec.data[offset+12 : offset+14]),
			attrID:     binary.LittleEndian.Uint16(rec.data[offset+14 : offset+16]),
		}

		// Parse name
		if attr.nameLength > 0 {
			nameStart := offset + int(attr.nameOffset)
			nameEnd := nameStart + int(attr.nameLength)*2
			if nameEnd <= len(rec.data) {
				utf16Chars := make([]uint16, attr.nameLength)
				for i := uint8(0); i < attr.nameLength; i++ {
					utf16Chars[i] = binary.LittleEndian.Uint16(rec.data[nameStart+int(i)*2:])
				}
				attr.name = string(utf16.Decode(utf16Chars))
			}
		}

		if attr.nonResident {
			if offset+64 <= len(rec.data) {
				attr.startVCN = binary.LittleEndian.Uint64(rec.data[offset+16 : offset+24])
				attr.endVCN = binary.LittleEndian.Uint64(rec.data[offset+24 : offset+32])
				attr.dataRunsOffset = binary.LittleEndian.Uint16(rec.data[offset+32 : offset+34])
				attr.allocatedSize = binary.LittleEndian.Uint64(rec.data[offset+40 : offset+48])
				attr.realSize = binary.LittleEndian.Uint64(rec.data[offset+48 : offset+56])
				attr.initSize = binary.LittleEndian.Uint64(rec.data[offset+56 : offset+64])

				// Parse data runs
				runOffset := offset + int(attr.dataRunsOffset)
				attr.dataRuns = f.parseDataRuns(rec.data[runOffset:])
			}
		} else {
			if offset+24 <= len(rec.data) {
				attr.valueLength = binary.LittleEndian.Uint32(rec.data[offset+16 : offset+20])
				attr.valueOffset = binary.LittleEndian.Uint16(rec.data[offset+20 : offset+22])

				valueStart := offset + int(attr.valueOffset)
				valueEnd := valueStart + int(attr.valueLength)
				if valueEnd <= len(rec.data) {
					attr.value = rec.data[valueStart:valueEnd]
				}
			}
		}

		attrs = append(attrs, attr)
		offset += int(length)
	}

	return attrs, nil
}

func (f *FS) parseDataRuns(data []byte) []dataRun {
	var runs []dataRun
	offset := 0
	currentLCN := int64(0)

	for offset < len(data) {
		header := data[offset]
		if header == 0 {
			break
		}

		lengthSize := int(header & 0x0F)
		offsetSize := int(header >> 4)

		if offset+1+lengthSize+offsetSize > len(data) {
			break
		}

		// Parse length
		length := uint64(0)
		for i := 0; i < lengthSize; i++ {
			length |= uint64(data[offset+1+i]) << (i * 8)
		}

		// Parse offset (signed)
		runOffset := int64(0)
		sparse := offsetSize == 0
		if !sparse {
			for i := 0; i < offsetSize; i++ {
				runOffset |= int64(data[offset+1+lengthSize+i]) << (i * 8)
			}
			// Sign extend if negative
			if data[offset+lengthSize+offsetSize]&0x80 != 0 {
				for i := offsetSize; i < 8; i++ {
					runOffset |= int64(0xFF) << (i * 8)
				}
			}
			currentLCN += runOffset
		}

		runs = append(runs, dataRun{
			length: length,
			offset: currentLCN,
			sparse: sparse,
		})

		offset += 1 + lengthSize + offsetSize
	}

	return runs
}

func (f *FS) readAttributeData(attr *attribute) ([]byte, error) {
	if !attr.nonResident {
		return attr.value, nil
	}

	var data []byte
	for _, run := range attr.dataRuns {
		if run.sparse {
			data = append(data, make([]byte, int(run.length)*f.clusterSize)...)
		} else {
			for i := uint64(0); i < run.length; i++ {
				cluster := uint64(run.offset) + i
				clusterData, err := f.readCluster(cluster)
				if err != nil {
					return nil, err
				}
				data = append(data, clusterData...)
			}
		}
	}

	if uint64(len(data)) > attr.realSize {
		data = data[:attr.realSize]
	}

	return data, nil
}

// loadMFT loads the entire MFT into memory for faster access
func (f *FS) loadMFT() error {
	if f.mftLoaded {
		return nil
	}

	mftRecord, err := f.readMFTRecord(0)
	if err != nil {
		return err
	}

	attrs, err := f.parseAttributes(mftRecord)
	if err != nil {
		return err
	}

	for _, attr := range attrs {
		if attr.attrType == attrData && attr.name == "" {
			f.mftData, err = f.readAttributeData(&attr)
			if err != nil {
				return err
			}
			f.mftLoaded = true
			return nil
		}
	}

	return fmt.Errorf("MFT $DATA attribute not found")
}

// fileNameAttr represents parsed $FILE_NAME attribute
type fileNameAttr struct {
	parentRef      uint64
	creationTime   time.Time
	modTime        time.Time
	mftModTime     time.Time
	accessTime     time.Time
	allocatedSize  uint64
	realSize       uint64
	flags          uint32
	nameType       uint8
	name           string
}

func parseFileNameAttr(data []byte) (*fileNameAttr, error) {
	if len(data) < 66 {
		return nil, fmt.Errorf("$FILE_NAME too small")
	}

	fn := &fileNameAttr{
		parentRef:     binary.LittleEndian.Uint64(data[0:8]) & 0x0000FFFFFFFFFFFF,
		allocatedSize: binary.LittleEndian.Uint64(data[40:48]),
		realSize:      binary.LittleEndian.Uint64(data[48:56]),
		flags:         binary.LittleEndian.Uint32(data[56:60]),
		nameType:      data[65],
	}

	fn.creationTime = windowsFileTimeToTime(binary.LittleEndian.Uint64(data[8:16]))
	fn.modTime = windowsFileTimeToTime(binary.LittleEndian.Uint64(data[16:24]))
	fn.mftModTime = windowsFileTimeToTime(binary.LittleEndian.Uint64(data[24:32]))
	fn.accessTime = windowsFileTimeToTime(binary.LittleEndian.Uint64(data[32:40]))

	nameLen := int(data[64])
	if len(data) < 66+nameLen*2 {
		return nil, fmt.Errorf("$FILE_NAME name truncated")
	}

	utf16Chars := make([]uint16, nameLen)
	for i := 0; i < nameLen; i++ {
		utf16Chars[i] = binary.LittleEndian.Uint16(data[66+i*2:])
	}
	fn.name = string(utf16.Decode(utf16Chars))

	return fn, nil
}

func windowsFileTimeToTime(ft uint64) time.Time {
	// Windows FILETIME is 100-nanosecond intervals since January 1, 1601
	const epochDiff = 116444736000000000 // Difference between 1601 and 1970 in 100-ns
	if ft < epochDiff {
		return time.Time{}
	}
	return time.Unix(0, int64((ft-epochDiff)*100))
}

// indexEntry represents a directory index entry
type indexEntry struct {
	mftRef        uint64
	entryLength   uint16
	contentLength uint16
	flags         uint32
	fileName      *fileNameAttr
}

func (f *FS) readDirectory(recordNum uint64) ([]indexEntry, error) {
	rec, err := f.readMFTRecord(recordNum)
	if err != nil {
		return nil, err
	}

	attrs, err := f.parseAttributes(rec)
	if err != nil {
		return nil, err
	}

	var entries []indexEntry

	// First, check $INDEX_ROOT for small directories
	for _, attr := range attrs {
		if attr.attrType == attrIndexRoot && attr.name == "$I30" {
			rootEntries, err := f.parseIndexRoot(attr.value)
			if err != nil {
				return nil, err
			}
			entries = append(entries, rootEntries...)
		}
	}

	// Then, check $INDEX_ALLOCATION for larger directories
	for _, attr := range attrs {
		if attr.attrType == attrIndexAllocation && attr.name == "$I30" {
			data, err := f.readAttributeData(&attr)
			if err != nil {
				return nil, err
			}
			allocEntries, err := f.parseIndexAllocation(data)
			if err != nil {
				return nil, err
			}
			entries = append(entries, allocEntries...)
		}
	}

	return entries, nil
}

func (f *FS) parseIndexRoot(data []byte) ([]indexEntry, error) {
	if len(data) < 32 {
		return nil, fmt.Errorf("$INDEX_ROOT too small")
	}

	// Index root header
	// attrType := binary.LittleEndian.Uint32(data[0:4])
	// collationRule := binary.LittleEndian.Uint32(data[4:8])
	// indexBlockSize := binary.LittleEndian.Uint32(data[8:12])
	// clustersPerIndexBlock := data[12]

	// Index node header
	entriesOffset := binary.LittleEndian.Uint32(data[16:20])
	// totalSize := binary.LittleEndian.Uint32(data[20:24])
	// allocatedSize := binary.LittleEndian.Uint32(data[24:28])
	// flags := data[28]

	return f.parseIndexEntries(data[16+entriesOffset:])
}

func (f *FS) parseIndexAllocation(data []byte) ([]indexEntry, error) {
	var allEntries []indexEntry

	for offset := 0; offset+int(f.indexRecordSize) <= len(data); offset += int(f.indexRecordSize) {
		block := data[offset : offset+int(f.indexRecordSize)]

		// Check INDX signature
		if !bytes.Equal(block[0:4], []byte("INDX")) {
			continue
		}

		// Apply fixup
		usaOffset := binary.LittleEndian.Uint16(block[4:6])
		usaCount := binary.LittleEndian.Uint16(block[6:8])
		if err := f.applyFixup(block, usaOffset, usaCount); err != nil {
			continue
		}

		// Parse index node header at offset 24
		entriesOffset := binary.LittleEndian.Uint32(block[24:28])
		// totalSize := binary.LittleEndian.Uint32(block[28:32])

		entries, err := f.parseIndexEntries(block[24+entriesOffset:])
		if err != nil {
			continue
		}
		allEntries = append(allEntries, entries...)
	}

	return allEntries, nil
}

func (f *FS) parseIndexEntries(data []byte) ([]indexEntry, error) {
	var entries []indexEntry
	offset := 0

	for offset+16 <= len(data) {
		entry := indexEntry{
			mftRef:        binary.LittleEndian.Uint64(data[offset : offset+8]),
			entryLength:   binary.LittleEndian.Uint16(data[offset+8 : offset+10]),
			contentLength: binary.LittleEndian.Uint16(data[offset+10 : offset+12]),
			flags:         binary.LittleEndian.Uint32(data[offset+12 : offset+16]),
		}

		if entry.entryLength == 0 {
			break
		}

		// Last entry flag
		if entry.flags&2 != 0 {
			break
		}

		// Parse file name if present
		if entry.contentLength > 0 && offset+16+int(entry.contentLength) <= len(data) {
			fn, err := parseFileNameAttr(data[offset+16 : offset+16+int(entry.contentLength)])
			if err == nil {
				entry.fileName = fn
			}
		}

		entries = append(entries, entry)
		offset += int(entry.entryLength)
	}

	return entries, nil
}

// fs.FS implementation

func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	if err := f.loadMFT(); err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if name == "." {
		rec, err := f.readMFTRecord(mftRecordRoot)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &ntfsDir{fs: f, record: rec, recordNum: mftRecordRoot, name: "."}, nil
	}

	recordNum, rec, fn, err := f.lookup(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if rec.flags&mftFlagDirectory != 0 {
		return &ntfsDir{fs: f, record: rec, recordNum: recordNum, name: path.Base(name), fileNameAttr: fn}, nil
	}

	return &ntfsFile{fs: f, record: rec, recordNum: recordNum, name: path.Base(name), fileNameAttr: fn}, nil
}

func (f *FS) lookup(name string) (uint64, *mftRecord, *fileNameAttr, error) {
	parts := strings.Split(name, "/")
	currentRecord := uint64(mftRecordRoot)

	var lastFN *fileNameAttr

	for _, part := range parts {
		entries, err := f.readDirectory(currentRecord)
		if err != nil {
			return 0, nil, nil, err
		}

		found := false
		for _, entry := range entries {
			if entry.fileName == nil {
				continue
			}
			// Skip DOS names
			if entry.fileName.nameType == fileNameDOS {
				continue
			}
			if strings.EqualFold(entry.fileName.name, part) {
				currentRecord = entry.mftRef & 0x0000FFFFFFFFFFFF
				lastFN = entry.fileName
				found = true
				break
			}
		}

		if !found {
			return 0, nil, nil, fs.ErrNotExist
		}
	}

	rec, err := f.readMFTRecord(currentRecord)
	if err != nil {
		return 0, nil, nil, err
	}

	return currentRecord, rec, lastFN, nil
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

// ntfsFile implements fs.File for regular files
type ntfsFile struct {
	fs           *FS
	record       *mftRecord
	recordNum    uint64
	name         string
	fileNameAttr *fileNameAttr
	data         []byte
	offset       int64
	loaded       bool
}

func (f *ntfsFile) Stat() (fs.FileInfo, error) {
	size := uint64(0)
	if f.fileNameAttr != nil {
		size = f.fileNameAttr.realSize
	}
	// Try to get actual size from $DATA attribute
	attrs, err := f.fs.parseAttributes(f.record)
	if err == nil {
		for _, attr := range attrs {
			if attr.attrType == attrData && attr.name == "" {
				if attr.nonResident {
					size = attr.realSize
				} else {
					size = uint64(attr.valueLength)
				}
				break
			}
		}
	}
	return &ntfsFileInfo{
		name:         f.name,
		size:         int64(size),
		fileNameAttr: f.fileNameAttr,
		isDir:        false,
		recordNum:    f.recordNum,
	}, nil
}

func (f *ntfsFile) Read(b []byte) (int, error) {
	if !f.loaded {
		attrs, err := f.fs.parseAttributes(f.record)
		if err != nil {
			return 0, err
		}

		for _, attr := range attrs {
			if attr.attrType == attrData && attr.name == "" {
				f.data, err = f.fs.readAttributeData(&attr)
				if err != nil {
					return 0, err
				}
				break
			}
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

func (f *ntfsFile) Close() error {
	f.data = nil
	return nil
}

// ntfsDir implements fs.File and fs.ReadDirFile for directories
type ntfsDir struct {
	fs           *FS
	record       *mftRecord
	recordNum    uint64
	name         string
	fileNameAttr *fileNameAttr
	entries      []fs.DirEntry
	offset       int
}

func (d *ntfsDir) Stat() (fs.FileInfo, error) {
	return &ntfsFileInfo{
		name:         d.name,
		fileNameAttr: d.fileNameAttr,
		isDir:        true,
		recordNum:    d.recordNum,
	}, nil
}

func (d *ntfsDir) Read(b []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *ntfsDir) Close() error {
	d.entries = nil
	return nil
}

func (d *ntfsDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.entries == nil {
		indexEntries, err := d.fs.readDirectory(d.recordNum)
		if err != nil {
			return nil, err
		}

		// Use map to deduplicate (prefer Win32/Both names over DOS names)
		seen := make(map[string]*ntfsDirEntry)
		for _, entry := range indexEntries {
			if entry.fileName == nil {
				continue
			}
			name := entry.fileName.name
			// Skip . and ..
			if name == "." || name == ".." {
				continue
			}
			// Skip DOS-only names if we have a better name
			existing, exists := seen[strings.ToLower(name)]
			if exists && existing.entry.fileName.nameType != fileNameDOS && entry.fileName.nameType == fileNameDOS {
				continue
			}
			seen[strings.ToLower(name)] = &ntfsDirEntry{fs: d.fs, entry: entry}
		}

		d.entries = make([]fs.DirEntry, 0, len(seen))
		for _, e := range seen {
			d.entries = append(d.entries, e)
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

// ntfsDirEntry implements fs.DirEntry
type ntfsDirEntry struct {
	fs    *FS
	entry indexEntry
}

func (e *ntfsDirEntry) Name() string { return e.entry.fileName.name }

func (e *ntfsDirEntry) IsDir() bool {
	return e.entry.fileName.flags&0x10000000 != 0 // FILE_ATTR_DIRECTORY
}

func (e *ntfsDirEntry) Type() fs.FileMode {
	if e.IsDir() {
		return fs.ModeDir
	}
	return 0
}

func (e *ntfsDirEntry) Info() (fs.FileInfo, error) {
	recordNum := e.entry.mftRef & 0x0000FFFFFFFFFFFF
	return &ntfsFileInfo{
		name:         e.entry.fileName.name,
		size:         int64(e.entry.fileName.realSize),
		fileNameAttr: e.entry.fileName,
		isDir:        e.IsDir(),
		recordNum:    recordNum,
	}, nil
}

// ntfsFileInfo implements fs.FileInfo
type ntfsFileInfo struct {
	name         string
	size         int64
	fileNameAttr *fileNameAttr
	isDir        bool
	recordNum    uint64
}

func (i *ntfsFileInfo) Name() string { return i.name }
func (i *ntfsFileInfo) Size() int64  { return i.size }
func (i *ntfsFileInfo) IsDir() bool  { return i.isDir }
func (i *ntfsFileInfo) Sys() any     { return nil }

func (i *ntfsFileInfo) ModTime() time.Time {
	if i.fileNameAttr != nil {
		return i.fileNameAttr.modTime
	}
	return time.Time{}
}

func (i *ntfsFileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(0444)
	if i.isDir {
		mode |= fs.ModeDir | 0111
	}
	return mode
}
