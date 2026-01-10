// Package detect identifies filesystem types from disk images.
package detect

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

// Type represents a filesystem type
type Type int

const (
	Unknown Type = iota
	FAT12
	FAT16
	FAT32
	NTFS
	Ext2
	Ext3
	Ext4
)

func (t Type) String() string {
	switch t {
	case FAT12:
		return "FAT12"
	case FAT16:
		return "FAT16"
	case FAT32:
		return "FAT32"
	case NTFS:
		return "NTFS"
	case Ext2:
		return "ext2"
	case Ext3:
		return "ext3"
	case Ext4:
		return "ext4"
	default:
		return "unknown"
	}
}

// IsFAT returns true if the type is any FAT variant
func (t Type) IsFAT() bool {
	return t == FAT12 || t == FAT16 || t == FAT32
}

// IsExt returns true if the type is any ext variant
func (t Type) IsExt() bool {
	return t == Ext2 || t == Ext3 || t == Ext4
}

// Detect identifies the filesystem type from a reader.
// It reads the necessary header bytes to identify the filesystem.
func Detect(r io.ReaderAt) (Type, error) {
	// Read first 4KB which should contain all magic bytes we need
	header := make([]byte, 4096)
	n, err := r.ReadAt(header, 0)
	if err != nil && err != io.EOF {
		return Unknown, fmt.Errorf("reading header: %w", err)
	}
	if n < 512 {
		return Unknown, fmt.Errorf("file too small: %d bytes", n)
	}

	// Check NTFS first (offset 3: "NTFS    ")
	if n >= 11 && bytes.Equal(header[3:11], []byte("NTFS    ")) {
		return NTFS, nil
	}

	// Check for ext2/3/4 superblock magic at offset 0x438 (1080)
	// The superblock starts at byte 1024
	if n >= 1082 {
		magic := binary.LittleEndian.Uint16(header[0x438:0x43A])
		if magic == 0xEF53 {
			return detectExtVersion(header[1024:]), nil
		}
	}

	// Check for FAT boot sector signature
	if header[510] == 0x55 && header[511] == 0xAA {
		return detectFATVersion(header), nil
	}

	return Unknown, nil
}

// detectFATVersion distinguishes between FAT12, FAT16, and FAT32
func detectFATVersion(header []byte) Type {
	// FAT32 has "FAT32   " at offset 82
	if len(header) >= 90 && bytes.Equal(header[82:90], []byte("FAT32   ")) {
		return FAT32
	}

	// FAT12/16 have "FAT12   " or "FAT16   " at offset 54
	if len(header) >= 62 {
		if bytes.Equal(header[54:62], []byte("FAT12   ")) {
			return FAT12
		}
		if bytes.Equal(header[54:62], []byte("FAT16   ")) {
			return FAT16
		}
	}

	// If no explicit label, calculate based on cluster count
	// This requires parsing the BPB (BIOS Parameter Block)
	if len(header) < 36 {
		return Unknown
	}

	bytesPerSector := binary.LittleEndian.Uint16(header[11:13])
	sectorsPerCluster := header[13]
	reservedSectors := binary.LittleEndian.Uint16(header[14:16])
	numFATs := header[16]
	rootEntryCount := binary.LittleEndian.Uint16(header[17:19])
	totalSectors16 := binary.LittleEndian.Uint16(header[19:21])
	fatSize16 := binary.LittleEndian.Uint16(header[22:24])
	totalSectors32 := binary.LittleEndian.Uint32(header[32:36])

	if bytesPerSector == 0 || sectorsPerCluster == 0 {
		return Unknown
	}

	var totalSectors uint32
	if totalSectors16 != 0 {
		totalSectors = uint32(totalSectors16)
	} else {
		totalSectors = totalSectors32
	}

	var fatSize uint32
	if fatSize16 != 0 {
		fatSize = uint32(fatSize16)
	} else if len(header) >= 40 {
		fatSize = binary.LittleEndian.Uint32(header[36:40])
	}

	rootDirSectors := ((uint32(rootEntryCount) * 32) + uint32(bytesPerSector) - 1) / uint32(bytesPerSector)
	dataSectors := totalSectors - (uint32(reservedSectors) + (uint32(numFATs) * fatSize) + rootDirSectors)
	countOfClusters := dataSectors / uint32(sectorsPerCluster)

	if countOfClusters < 4085 {
		return FAT12
	} else if countOfClusters < 65525 {
		return FAT16
	}
	return FAT32
}

// detectExtVersion distinguishes between ext2, ext3, and ext4
// superblock is the data starting at byte 1024 of the image
func detectExtVersion(superblock []byte) Type {
	if len(superblock) < 100 {
		return Ext2
	}

	// s_feature_compat at offset 0x5C (92)
	featureCompat := binary.LittleEndian.Uint32(superblock[0x5C:0x60])
	// s_feature_incompat at offset 0x60 (96)
	featureIncompat := binary.LittleEndian.Uint32(superblock[0x60:0x64])
	// s_feature_ro_compat at offset 0x64 (100)
	// featureROCompat := binary.LittleEndian.Uint32(superblock[0x64:0x68])

	// ext4 incompatible features
	const (
		EXT4_FEATURE_INCOMPAT_64BIT      = 0x0080
		EXT4_FEATURE_INCOMPAT_EXTENTS    = 0x0040
		EXT4_FEATURE_INCOMPAT_FLEX_BG    = 0x0200
		EXT3_FEATURE_COMPAT_HAS_JOURNAL  = 0x0004
	)

	// Check for ext4-specific features
	ext4Features := EXT4_FEATURE_INCOMPAT_64BIT | EXT4_FEATURE_INCOMPAT_EXTENTS | EXT4_FEATURE_INCOMPAT_FLEX_BG
	if featureIncompat&uint32(ext4Features) != 0 {
		return Ext4
	}

	// Check for journal (ext3+)
	if featureCompat&EXT3_FEATURE_COMPAT_HAS_JOURNAL != 0 {
		return Ext3
	}

	return Ext2
}
