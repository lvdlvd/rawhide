// Package ext implements read-only ext2/ext3/ext4 filesystem support.
package ext

import (
	"encoding/binary"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"

	"github.com/lvdlvd/rawhide/fsys"
)

const (
	superblockOffset = 1024
	superblockSize   = 1024
	extMagic         = 0xEF53

	// Inode flags
	inodeFlagExtents = 0x00080000

	// Feature flags
	featureIncompatExtents = 0x0040
	featureIncompat64Bit   = 0x0080
	featureCompatHasJournal = 0x0004
)

// FS implements a read-only ext2/3/4 filesystem
type FS struct {
	r         io.ReaderAt
	size      int64
	sb        superblock
	blockSize uint32
	typ       string
}

type superblock struct {
	inodesCount        uint32
	blocksCount        uint64
	freeBlocksCount    uint64
	freeInodesCount    uint32
	firstDataBlock     uint32
	logBlockSize       uint32
	logClusterSize     uint32
	blocksPerGroup     uint32
	clustersPerGroup   uint32
	inodesPerGroup     uint32
	mtime              uint32
	wtime              uint32
	mntCount           uint16
	maxMntCount        int16
	magic              uint16
	state              uint16
	errors             uint16
	minorRevLevel      uint16
	lastcheck          uint32
	checkinterval      uint32
	creatorOS          uint32
	revLevel           uint32
	defResuid          uint16
	defResgid          uint16
	firstIno           uint32
	inodeSize          uint16
	blockGroupNr       uint16
	featureCompat      uint32
	featureIncompat    uint32
	featureROCompat    uint32
	uuid               [16]byte
	volumeName         [16]byte
	descSize           uint16
	groupCount         uint32
}

type blockGroupDescriptor struct {
	blockBitmap     uint64
	inodeBitmap     uint64
	inodeTable      uint64
	freeBlocksCount uint32
	freeInodesCount uint32
	usedDirsCount   uint32
}

type inode struct {
	mode        uint16
	uid         uint16
	size        uint64
	atime       uint32
	ctime       uint32
	mtime       uint32
	dtime       uint32
	gid         uint16
	linksCount  uint16
	blocks      uint64
	flags       uint32
	block       [60]byte // 15 * 4 bytes for block pointers or extent tree
	generation  uint32
	fileACL     uint64
	dirACL      uint32
}

// Open opens an ext2/3/4 filesystem from the given reader
func Open(r io.ReaderAt, size int64) (fsys.FS, error) {
	sbData := make([]byte, superblockSize)
	if _, err := r.ReadAt(sbData, superblockOffset); err != nil {
		return nil, fmt.Errorf("reading superblock: %w", err)
	}

	magic := binary.LittleEndian.Uint16(sbData[0x38:0x3A])
	if magic != extMagic {
		return nil, nil // Not an ext filesystem
	}

	fs := &FS{r: r, size: size}
	if err := fs.parseSuperblock(sbData); err != nil {
		return nil, err
	}

	return fs, nil
}

func (f *FS) parseSuperblock(data []byte) error {
	f.sb.inodesCount = binary.LittleEndian.Uint32(data[0x00:0x04])
	f.sb.blocksCount = uint64(binary.LittleEndian.Uint32(data[0x04:0x08]))
	f.sb.freeBlocksCount = uint64(binary.LittleEndian.Uint32(data[0x0C:0x10]))
	f.sb.freeInodesCount = binary.LittleEndian.Uint32(data[0x10:0x14])
	f.sb.firstDataBlock = binary.LittleEndian.Uint32(data[0x14:0x18])
	f.sb.logBlockSize = binary.LittleEndian.Uint32(data[0x18:0x1C])
	f.sb.blocksPerGroup = binary.LittleEndian.Uint32(data[0x20:0x24])
	f.sb.inodesPerGroup = binary.LittleEndian.Uint32(data[0x28:0x2C])
	f.sb.magic = binary.LittleEndian.Uint16(data[0x38:0x3A])
	f.sb.revLevel = binary.LittleEndian.Uint32(data[0x4C:0x50])
	f.sb.firstIno = binary.LittleEndian.Uint32(data[0x54:0x58])
	f.sb.inodeSize = binary.LittleEndian.Uint16(data[0x58:0x5A])
	f.sb.featureCompat = binary.LittleEndian.Uint32(data[0x5C:0x60])
	f.sb.featureIncompat = binary.LittleEndian.Uint32(data[0x60:0x64])
	f.sb.featureROCompat = binary.LittleEndian.Uint32(data[0x64:0x68])
	copy(f.sb.uuid[:], data[0x68:0x78])
	copy(f.sb.volumeName[:], data[0x78:0x88])

	f.blockSize = 1024 << f.sb.logBlockSize

	// Default inode size for rev 0
	if f.sb.revLevel == 0 {
		f.sb.inodeSize = 128
	}

	// Descriptor size for 64-bit feature
	if f.sb.featureIncompat&featureIncompat64Bit != 0 {
		f.sb.descSize = binary.LittleEndian.Uint16(data[0xFE:0x100])
		if f.sb.descSize == 0 {
			f.sb.descSize = 64
		}
		// Get high 32 bits of block count
		high := binary.LittleEndian.Uint32(data[0x150:0x154])
		f.sb.blocksCount |= uint64(high) << 32
	} else {
		f.sb.descSize = 32
	}

	// Calculate group count
	f.sb.groupCount = uint32((f.sb.blocksCount-uint64(f.sb.firstDataBlock)+uint64(f.sb.blocksPerGroup)-1) / uint64(f.sb.blocksPerGroup))

	// Determine filesystem type
	if f.sb.featureIncompat&(featureIncompatExtents|featureIncompat64Bit) != 0 {
		f.typ = "ext4"
	} else if f.sb.featureCompat&featureCompatHasJournal != 0 {
		f.typ = "ext3"
	} else {
		f.typ = "ext2"
	}

	return nil
}

func (f *FS) Type() string  { return f.typ }
func (f *FS) Close() error  { return nil }
func (f *FS) BaseReader() io.ReaderAt { return f.r }

// FreeBlocks returns the list of free byte ranges in the ext filesystem.
// Free blocks are identified by 0 bits in the block bitmaps.
func (f *FS) FreeBlocks() ([]fsys.Range, error) {
	var ranges []fsys.Range
	blockSize := int64(f.blockSize)

	// Iterate through all block groups
	for group := uint32(0); group < f.sb.groupCount; group++ {
		bgd, err := f.readBlockGroupDescriptor(group)
		if err != nil {
			return nil, fmt.Errorf("reading block group descriptor %d: %w", group, err)
		}

		// Read the block bitmap for this group
		bitmap, err := f.readBlock(bgd.blockBitmap)
		if err != nil {
			return nil, fmt.Errorf("reading block bitmap for group %d: %w", group, err)
		}

		// Calculate the first block number in this group
		firstBlock := uint64(f.sb.firstDataBlock) + uint64(group)*uint64(f.sb.blocksPerGroup)

		// Number of blocks in this group (last group may have fewer)
		blocksInGroup := uint64(f.sb.blocksPerGroup)
		remainingBlocks := f.sb.blocksCount - firstBlock
		if blocksInGroup > remainingBlocks {
			blocksInGroup = remainingBlocks
		}

		// Scan bitmap for free blocks
		var inFreeRange bool
		var rangeStart int64

		for i := uint64(0); i < blocksInGroup; i++ {
			byteIndex := i / 8
			bitIndex := i % 8

			if int(byteIndex) >= len(bitmap) {
				break
			}

			// In ext2/3/4, bit=0 means free, bit=1 means allocated
			isFree := (bitmap[byteIndex] & (1 << bitIndex)) == 0
			blockNum := firstBlock + i
			offset := int64(blockNum) * blockSize

			if isFree && !inFreeRange {
				rangeStart = offset
				inFreeRange = true
			} else if !isFree && inFreeRange {
				ranges = append(ranges, fsys.Range{Start: rangeStart, End: offset})
				inFreeRange = false
			}
		}

		// Close range at end of group if still in one
		if inFreeRange {
			endBlock := firstBlock + blocksInGroup
			ranges = append(ranges, fsys.Range{Start: rangeStart, End: int64(endBlock) * blockSize})
			inFreeRange = false
		}
	}

	// Merge adjacent ranges from different groups
	ranges = mergeRanges(ranges)

	return ranges, nil
}

// mergeRanges combines adjacent ranges
func mergeRanges(ranges []fsys.Range) []fsys.Range {
	if len(ranges) <= 1 {
		return ranges
	}

	merged := make([]fsys.Range, 0, len(ranges))
	current := ranges[0]

	for i := 1; i < len(ranges); i++ {
		if ranges[i].Start == current.End {
			// Adjacent, extend current range
			current.End = ranges[i].End
		} else {
			merged = append(merged, current)
			current = ranges[i]
		}
	}
	merged = append(merged, current)

	return merged
}

// FileExtents returns the physical extents for a file
func (f *FS) FileExtents(name string) ([]fsys.Extent, error) {
	if name == "." || name == "" {
		return nil, fmt.Errorf("cannot get extents for root directory")
	}

	inodeNum, ino, err := f.lookup(name)
	if err != nil {
		return nil, err
	}
	_ = inodeNum

	// Check if it's a directory
	if ino.mode&0xF000 == 0x4000 {
		return nil, fmt.Errorf("cannot get extents for directory")
	}

	fileSize := int64(ino.size)
	if ino.flags&inodeFlagExtents != 0 {
		return f.getExtentTreeExtents(ino, fileSize)
	}
	return f.getBlockPointerExtents(ino, fileSize)
}

// getExtentTreeExtents returns extents from an extent tree
func (f *FS) getExtentTreeExtents(ino inode, fileSize int64) ([]fsys.Extent, error) {
	var extents []fsys.Extent
	blockSize := int64(f.blockSize)
	remaining := fileSize

	err := f.walkExtentTree(ino.block[:], func(e extent) error {
		if remaining <= 0 {
			return io.EOF
		}

		startBlock := uint64(e.startLo) | (uint64(e.startHi) << 32)
		length := uint16(e.len)
		if length > 0x8000 {
			length -= 0x8000 // Uninitialized extent
		}

		logicalBlock := uint64(e.block)
		extentBytes := int64(length) * blockSize
		if extentBytes > remaining {
			extentBytes = remaining
		}

		extents = append(extents, fsys.Extent{
			Logical:  int64(logicalBlock) * blockSize,
			Physical: int64(startBlock) * blockSize,
			Length:   extentBytes,
		})

		remaining -= extentBytes
		return nil
	})

	if err != nil && err != io.EOF {
		return nil, err
	}

	return extents, nil
}

// getBlockPointerExtents returns extents from block pointers
func (f *FS) getBlockPointerExtents(ino inode, fileSize int64) ([]fsys.Extent, error) {
	var extents []fsys.Extent
	blockSize := int64(f.blockSize)
	blocksNeeded := (fileSize + blockSize - 1) / blockSize
	logicalOffset := int64(0)
	remaining := fileSize

	var currentExtent *fsys.Extent

	addBlock := func(blockNum uint64) {
		if remaining <= 0 {
			return
		}
		physOffset := int64(blockNum) * blockSize
		extentLen := blockSize
		if extentLen > remaining {
			extentLen = remaining
		}

		// Try to extend current extent if contiguous
		if currentExtent != nil &&
			currentExtent.Physical+currentExtent.Length == physOffset {
			currentExtent.Length += extentLen
		} else {
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
	}

	// Direct blocks (0-11)
	for i := 0; i < 12 && logicalOffset/blockSize < blocksNeeded; i++ {
		blockNum := binary.LittleEndian.Uint32(ino.block[i*4 : (i+1)*4])
		if blockNum == 0 {
			continue
		}
		addBlock(uint64(blockNum))
	}

	// Single indirect (12)
	if logicalOffset/blockSize < blocksNeeded {
		indirectBlock := binary.LittleEndian.Uint32(ino.block[48:52])
		if indirectBlock != 0 {
			if err := f.walkIndirectExtents(uint64(indirectBlock), 1, addBlock); err != nil {
				return nil, err
			}
		}
	}

	// Double indirect (13)
	if logicalOffset/blockSize < blocksNeeded {
		doubleIndirectBlock := binary.LittleEndian.Uint32(ino.block[52:56])
		if doubleIndirectBlock != 0 {
			if err := f.walkIndirectExtents(uint64(doubleIndirectBlock), 2, addBlock); err != nil {
				return nil, err
			}
		}
	}

	// Triple indirect (14)
	if logicalOffset/blockSize < blocksNeeded {
		tripleIndirectBlock := binary.LittleEndian.Uint32(ino.block[56:60])
		if tripleIndirectBlock != 0 {
			if err := f.walkIndirectExtents(uint64(tripleIndirectBlock), 3, addBlock); err != nil {
				return nil, err
			}
		}
	}

	if currentExtent != nil {
		extents = append(extents, *currentExtent)
	}

	return extents, nil
}

func (f *FS) walkIndirectExtents(block uint64, level int, addBlock func(uint64)) error {
	blockData, err := f.readBlock(block)
	if err != nil {
		return err
	}

	pointersPerBlock := int(f.blockSize / 4)
	for i := 0; i < pointersPerBlock; i++ {
		ptr := binary.LittleEndian.Uint32(blockData[i*4 : (i+1)*4])
		if ptr == 0 {
			continue
		}

		if level == 1 {
			addBlock(uint64(ptr))
		} else {
			if err := f.walkIndirectExtents(uint64(ptr), level-1, addBlock); err != nil {
				return err
			}
		}
	}

	return nil
}

func (f *FS) blockOffset(block uint64) int64 {
	return int64(block) * int64(f.blockSize)
}

func (f *FS) readBlock(block uint64) ([]byte, error) {
	data := make([]byte, f.blockSize)
	offset := f.blockOffset(block)
	if _, err := f.r.ReadAt(data, offset); err != nil {
		return nil, err
	}
	return data, nil
}

func (f *FS) readBlockGroupDescriptor(group uint32) (blockGroupDescriptor, error) {
	// Block group descriptors start at block 1 (or 2 if block size is 1024)
	descBlock := uint64(f.sb.firstDataBlock + 1)
	descOffset := f.blockOffset(descBlock) + int64(group)*int64(f.sb.descSize)

	data := make([]byte, f.sb.descSize)
	if _, err := f.r.ReadAt(data, descOffset); err != nil {
		return blockGroupDescriptor{}, err
	}

	bgd := blockGroupDescriptor{
		blockBitmap:     uint64(binary.LittleEndian.Uint32(data[0x00:0x04])),
		inodeBitmap:     uint64(binary.LittleEndian.Uint32(data[0x04:0x08])),
		inodeTable:      uint64(binary.LittleEndian.Uint32(data[0x08:0x0C])),
		freeBlocksCount: uint32(binary.LittleEndian.Uint16(data[0x0C:0x0E])),
		freeInodesCount: uint32(binary.LittleEndian.Uint16(data[0x0E:0x10])),
		usedDirsCount:   uint32(binary.LittleEndian.Uint16(data[0x10:0x12])),
	}

	// 64-bit extensions
	if f.sb.featureIncompat&featureIncompat64Bit != 0 && f.sb.descSize >= 64 {
		bgd.blockBitmap |= uint64(binary.LittleEndian.Uint32(data[0x20:0x24])) << 32
		bgd.inodeBitmap |= uint64(binary.LittleEndian.Uint32(data[0x24:0x28])) << 32
		bgd.inodeTable |= uint64(binary.LittleEndian.Uint32(data[0x28:0x2C])) << 32
	}

	return bgd, nil
}

func (f *FS) readInode(inodeNum uint32) (inode, error) {
	if inodeNum == 0 {
		return inode{}, fmt.Errorf("invalid inode number 0")
	}

	group := (inodeNum - 1) / f.sb.inodesPerGroup
	index := (inodeNum - 1) % f.sb.inodesPerGroup

	bgd, err := f.readBlockGroupDescriptor(group)
	if err != nil {
		return inode{}, err
	}

	inodeOffset := f.blockOffset(bgd.inodeTable) + int64(index)*int64(f.sb.inodeSize)
	data := make([]byte, f.sb.inodeSize)
	if _, err := f.r.ReadAt(data, inodeOffset); err != nil {
		return inode{}, err
	}

	ino := inode{
		mode:       binary.LittleEndian.Uint16(data[0x00:0x02]),
		uid:        binary.LittleEndian.Uint16(data[0x02:0x04]),
		size:       uint64(binary.LittleEndian.Uint32(data[0x04:0x08])),
		atime:      binary.LittleEndian.Uint32(data[0x08:0x0C]),
		ctime:      binary.LittleEndian.Uint32(data[0x0C:0x10]),
		mtime:      binary.LittleEndian.Uint32(data[0x10:0x14]),
		dtime:      binary.LittleEndian.Uint32(data[0x14:0x18]),
		gid:        binary.LittleEndian.Uint16(data[0x18:0x1A]),
		linksCount: binary.LittleEndian.Uint16(data[0x1A:0x1C]),
		blocks:     uint64(binary.LittleEndian.Uint32(data[0x1C:0x20])),
		flags:      binary.LittleEndian.Uint32(data[0x20:0x24]),
	}
	copy(ino.block[:], data[0x28:0x64])

	// Size high bits (for large files and directories)
	if ino.mode&0xF000 == 0x8000 || ino.mode&0xF000 == 0x4000 {
		ino.size |= uint64(binary.LittleEndian.Uint32(data[0x6C:0x70])) << 32
	}

	return ino, nil
}

// readInodeData reads all data blocks for an inode
func (f *FS) readInodeData(ino inode, maxSize int64) ([]byte, error) {
	if maxSize == 0 {
		maxSize = int64(ino.size)
	}
	if maxSize > int64(ino.size) {
		maxSize = int64(ino.size)
	}

	if ino.flags&inodeFlagExtents != 0 {
		return f.readExtents(ino, maxSize)
	}
	return f.readBlockPointers(ino, maxSize)
}

// readBlockPointers reads data using traditional block pointers
func (f *FS) readBlockPointers(ino inode, maxSize int64) ([]byte, error) {
	var data []byte
	blocksNeeded := (maxSize + int64(f.blockSize) - 1) / int64(f.blockSize)
	blocksRead := int64(0)

	// Direct blocks (0-11)
	for i := 0; i < 12 && blocksRead < blocksNeeded; i++ {
		blockNum := binary.LittleEndian.Uint32(ino.block[i*4 : (i+1)*4])
		if blockNum == 0 {
			continue
		}
		block, err := f.readBlock(uint64(blockNum))
		if err != nil {
			return nil, err
		}
		data = append(data, block...)
		blocksRead++
	}

	// Single indirect (12)
	if blocksRead < blocksNeeded {
		indirectBlock := binary.LittleEndian.Uint32(ino.block[48:52])
		if indirectBlock != 0 {
			moreData, err := f.readIndirectBlocks(uint64(indirectBlock), 1, blocksNeeded-blocksRead)
			if err != nil {
				return nil, err
			}
			data = append(data, moreData...)
			blocksRead += int64(len(moreData)) / int64(f.blockSize)
		}
	}

	// Double indirect (13)
	if blocksRead < blocksNeeded {
		doubleIndirectBlock := binary.LittleEndian.Uint32(ino.block[52:56])
		if doubleIndirectBlock != 0 {
			moreData, err := f.readIndirectBlocks(uint64(doubleIndirectBlock), 2, blocksNeeded-blocksRead)
			if err != nil {
				return nil, err
			}
			data = append(data, moreData...)
			blocksRead += int64(len(moreData)) / int64(f.blockSize)
		}
	}

	// Triple indirect (14)
	if blocksRead < blocksNeeded {
		tripleIndirectBlock := binary.LittleEndian.Uint32(ino.block[56:60])
		if tripleIndirectBlock != 0 {
			moreData, err := f.readIndirectBlocks(uint64(tripleIndirectBlock), 3, blocksNeeded-blocksRead)
			if err != nil {
				return nil, err
			}
			data = append(data, moreData...)
		}
	}

	if int64(len(data)) > maxSize {
		data = data[:maxSize]
	}
	return data, nil
}

func (f *FS) readIndirectBlocks(block uint64, level int, maxBlocks int64) ([]byte, error) {
	blockData, err := f.readBlock(block)
	if err != nil {
		return nil, err
	}

	var data []byte
	pointersPerBlock := int(f.blockSize / 4)
	blocksRead := int64(0)

	for i := 0; i < pointersPerBlock && blocksRead < maxBlocks; i++ {
		ptr := binary.LittleEndian.Uint32(blockData[i*4 : (i+1)*4])
		if ptr == 0 {
			continue
		}

		if level == 1 {
			blk, err := f.readBlock(uint64(ptr))
			if err != nil {
				return nil, err
			}
			data = append(data, blk...)
			blocksRead++
		} else {
			moreData, err := f.readIndirectBlocks(uint64(ptr), level-1, maxBlocks-blocksRead)
			if err != nil {
				return nil, err
			}
			data = append(data, moreData...)
			blocksRead += int64(len(moreData)) / int64(f.blockSize)
		}
	}

	return data, nil
}

// Extent tree structures
type extentHeader struct {
	magic      uint16
	entries    uint16
	max        uint16
	depth      uint16
	generation uint32
}

type extentIdx struct {
	block    uint32
	leafLo   uint32
	leafHi   uint16
	unused   uint16
}

type extent struct {
	block   uint32
	len     uint16
	startHi uint16
	startLo uint32
}

func (f *FS) readExtents(ino inode, maxSize int64) ([]byte, error) {
	var data []byte

	err := f.walkExtentTree(ino.block[:], func(e extent) error {
		if int64(len(data)) >= maxSize {
			return io.EOF
		}

		startBlock := uint64(e.startLo) | (uint64(e.startHi) << 32)
		length := uint16(e.len)
		if length > 0x8000 {
			// Uninitialized extent
			length -= 0x8000
		}

		for i := uint16(0); i < length; i++ {
			if int64(len(data)) >= maxSize {
				break
			}
			block, err := f.readBlock(startBlock + uint64(i))
			if err != nil {
				return err
			}
			data = append(data, block...)
		}
		return nil
	})

	if err != nil && err != io.EOF {
		return nil, err
	}

	if int64(len(data)) > maxSize {
		data = data[:maxSize]
	}
	return data, nil
}

func (f *FS) walkExtentTree(data []byte, fn func(extent) error) error {
	hdr := extentHeader{
		magic:   binary.LittleEndian.Uint16(data[0:2]),
		entries: binary.LittleEndian.Uint16(data[2:4]),
		max:     binary.LittleEndian.Uint16(data[4:6]),
		depth:   binary.LittleEndian.Uint16(data[6:8]),
	}

	if hdr.magic != 0xF30A {
		return fmt.Errorf("invalid extent magic: %04x", hdr.magic)
	}

	if hdr.depth == 0 {
		// Leaf node - actual extents
		for i := uint16(0); i < hdr.entries; i++ {
			offset := 12 + int(i)*12
			e := extent{
				block:   binary.LittleEndian.Uint32(data[offset : offset+4]),
				len:     binary.LittleEndian.Uint16(data[offset+4 : offset+6]),
				startHi: binary.LittleEndian.Uint16(data[offset+6 : offset+8]),
				startLo: binary.LittleEndian.Uint32(data[offset+8 : offset+12]),
			}
			if err := fn(e); err != nil {
				return err
			}
		}
	} else {
		// Index node
		for i := uint16(0); i < hdr.entries; i++ {
			offset := 12 + int(i)*12
			idx := extentIdx{
				block:  binary.LittleEndian.Uint32(data[offset : offset+4]),
				leafLo: binary.LittleEndian.Uint32(data[offset+4 : offset+8]),
				leafHi: binary.LittleEndian.Uint16(data[offset+8 : offset+10]),
			}
			leafBlock := uint64(idx.leafLo) | (uint64(idx.leafHi) << 32)
			blockData, err := f.readBlock(leafBlock)
			if err != nil {
				return err
			}
			if err := f.walkExtentTree(blockData, fn); err != nil {
				return err
			}
		}
	}

	return nil
}

// Directory entry structure
type dirEntry struct {
	inode    uint32
	recLen   uint16
	nameLen  uint8
	fileType uint8
	name     string
}

func (f *FS) readDirectory(ino inode) ([]dirEntry, error) {
	data, err := f.readInodeData(ino, 0)
	if err != nil {
		return nil, err
	}

	var entries []dirEntry
	offset := 0

	for offset < len(data) {
		if offset+8 > len(data) {
			break
		}

		inodeNum := binary.LittleEndian.Uint32(data[offset : offset+4])
		recLen := binary.LittleEndian.Uint16(data[offset+4 : offset+6])
		nameLen := data[offset+6]
		fileType := data[offset+7]

		if recLen == 0 || recLen < 8 {
			break
		}

		if inodeNum != 0 && int(nameLen) > 0 {
			nameEnd := offset + 8 + int(nameLen)
			if nameEnd > len(data) {
				nameEnd = len(data)
			}
			name := string(data[offset+8 : nameEnd])

			entries = append(entries, dirEntry{
				inode:    inodeNum,
				recLen:   recLen,
				nameLen:  nameLen,
				fileType: fileType,
				name:     name,
			})
		}

		offset += int(recLen)
	}

	return entries, nil
}

// fs.FS implementation

const rootInode = 2

func (f *FS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}

	if name == "." {
		ino, err := f.readInode(rootInode)
		if err != nil {
			return nil, &fs.PathError{Op: "open", Path: name, Err: err}
		}
		return &extDir{fs: f, inode: ino, inodeNum: rootInode, name: "."}, nil
	}

	inodeNum, ino, err := f.lookup(name)
	if err != nil {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}

	if ino.mode&0xF000 == 0x4000 {
		return &extDir{fs: f, inode: ino, inodeNum: inodeNum, name: path.Base(name)}, nil
	}

	return &extFile{fs: f, inode: ino, inodeNum: inodeNum, name: path.Base(name)}, nil
}

func (f *FS) lookup(name string) (uint32, inode, error) {
	parts := strings.Split(name, "/")
	currentInode := uint32(rootInode)

	for _, part := range parts {
		ino, err := f.readInode(currentInode)
		if err != nil {
			return 0, inode{}, err
		}

		if ino.mode&0xF000 != 0x4000 {
			return 0, inode{}, fs.ErrNotExist
		}

		entries, err := f.readDirectory(ino)
		if err != nil {
			return 0, inode{}, err
		}

		found := false
		for _, e := range entries {
			if e.name == part {
				currentInode = e.inode
				found = true
				break
			}
		}

		if !found {
			return 0, inode{}, fs.ErrNotExist
		}
	}

	ino, err := f.readInode(currentInode)
	if err != nil {
		return 0, inode{}, err
	}

	return currentInode, ino, nil
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

// extFile implements fs.File for regular files
type extFile struct {
	fs       *FS
	inode    inode
	inodeNum uint32
	name     string
	data     []byte
	offset   int64
	loaded   bool
}

func (f *extFile) Stat() (fs.FileInfo, error) {
	return &extFileInfo{inode: f.inode, inodeNum: f.inodeNum, name: f.name}, nil
}

func (f *extFile) Read(b []byte) (int, error) {
	if !f.loaded {
		var err error
		f.data, err = f.fs.readInodeData(f.inode, 0)
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

func (f *extFile) Close() error {
	f.data = nil
	return nil
}

// extDir implements fs.File and fs.ReadDirFile for directories
type extDir struct {
	fs       *FS
	inode    inode
	inodeNum uint32
	name     string
	entries  []fs.DirEntry
	offset   int
}

func (d *extDir) Stat() (fs.FileInfo, error) {
	return &extFileInfo{inode: d.inode, inodeNum: d.inodeNum, name: d.name}, nil
}

func (d *extDir) Read(b []byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}

func (d *extDir) Close() error {
	d.entries = nil
	return nil
}

func (d *extDir) ReadDir(n int) ([]fs.DirEntry, error) {
	if d.entries == nil {
		rawEntries, err := d.fs.readDirectory(d.inode)
		if err != nil {
			return nil, err
		}

		d.entries = make([]fs.DirEntry, 0, len(rawEntries))
		for _, e := range rawEntries {
			if e.name == "." || e.name == ".." {
				continue
			}
			d.entries = append(d.entries, &extDirEntry{fs: d.fs, entry: e})
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

// extDirEntry implements fs.DirEntry
type extDirEntry struct {
	fs    *FS
	entry dirEntry
}

func (e *extDirEntry) Name() string { return e.entry.name }

func (e *extDirEntry) IsDir() bool {
	// File type in directory entry: 2 = directory
	return e.entry.fileType == 2
}

func (e *extDirEntry) Type() fs.FileMode {
	if e.IsDir() {
		return fs.ModeDir
	}
	switch e.entry.fileType {
	case 7: // Symlink
		return fs.ModeSymlink
	default:
		return 0
	}
}

func (e *extDirEntry) Info() (fs.FileInfo, error) {
	ino, err := e.fs.readInode(e.entry.inode)
	if err != nil {
		return nil, err
	}
	return &extFileInfo{inode: ino, inodeNum: e.entry.inode, name: e.entry.name}, nil
}

// extFileInfo implements fs.FileInfo and fsys.FileInfo
type extFileInfo struct {
	inode    inode
	inodeNum uint32
	name     string
}

func (i *extFileInfo) Name() string       { return i.name }
func (i *extFileInfo) Size() int64        { return int64(i.inode.size) }
func (i *extFileInfo) ModTime() time.Time { return time.Unix(int64(i.inode.mtime), 0) }
func (i *extFileInfo) IsDir() bool        { return i.inode.mode&0xF000 == 0x4000 }
func (i *extFileInfo) Sys() any           { return nil }
func (i *extFileInfo) Inode() uint64      { return uint64(i.inodeNum) }

func (i *extFileInfo) Mode() fs.FileMode {
	mode := fs.FileMode(i.inode.mode & 0777)
	switch i.inode.mode & 0xF000 {
	case 0x4000:
		mode |= fs.ModeDir
	case 0xA000:
		mode |= fs.ModeSymlink
	case 0x6000:
		mode |= fs.ModeDevice
	case 0x2000:
		mode |= fs.ModeDevice | fs.ModeCharDevice
	case 0x1000:
		mode |= fs.ModeNamedPipe
	case 0xC000:
		mode |= fs.ModeSocket
	}
	return mode
}
