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
	MBR // Master Boot Record partition table
	GPT // GUID Partition Table
	APFS
	HFSPlus
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
	case MBR:
		return "MBR"
	case GPT:
		return "GPT"
	case APFS:
		return "APFS"
	case HFSPlus:
		return "HFS+"
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

// IsPartitionTable returns true if the type is a partition table format
func (t Type) IsPartitionTable() bool {
	return t == MBR || t == GPT
}

// IsApple returns true if the type is an Apple filesystem
func (t Type) IsApple() bool {
	return t == APFS || t == HFSPlus
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

	// Check for GPT (GUID Partition Table) - "EFI PART" at LBA 1 (offset 512)
	if n >= 520 && bytes.Equal(header[512:520], []byte("EFI PART")) {
		return GPT, nil
	}

	// Check for APFS container superblock - "NXSB" at offset 32
	if n >= 36 && binary.LittleEndian.Uint32(header[32:36]) == 0x4253584E {
		return APFS, nil
	}

	// Check for HFS+ volume header at offset 1024
	// Signature is 'H+' (0x482B) or 'HX' (0x4858) in big-endian
	if n >= 1026 {
		sig := binary.BigEndian.Uint16(header[1024:1026])
		if sig == 0x482B || sig == 0x4858 {
			return HFSPlus, nil
		}
	}

	// Check NTFS (offset 3: "NTFS    ")
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

	// Check for FAT boot sector signature or MBR partition table
	if header[510] == 0x55 && header[511] == 0xAA {
		// Check if this looks like a partition table (MBR)
		// MBR has partition entries at offset 446-509
		if isMBRPartitionTable(header) {
			return MBR, nil
		}
		// Otherwise it's a FAT filesystem
		return detectFATVersion(header), nil
	}

	return Unknown, nil
}

// isMBRPartitionTable checks if the boot sector contains a valid MBR partition table
func isMBRPartitionTable(header []byte) bool {
	if len(header) < 512 {
		return false
	}

	// Check for at least one valid partition entry
	// MBR partition table starts at offset 446, each entry is 16 bytes
	validPartitions := 0
	for i := 0; i < 4; i++ {
		entry := header[446+i*16 : 446+(i+1)*16]

		// Check boot flag (must be 0x00 or 0x80)
		bootFlag := entry[0]
		if bootFlag != 0x00 && bootFlag != 0x80 {
			continue
		}

		// Check partition type (byte 4)
		partType := entry[4]
		if partType == 0x00 {
			continue // Empty entry
		}

		// Check if it's a known partition type
		if isKnownPartitionType(partType) {
			// Verify LBA start and size are non-zero
			lbaStart := binary.LittleEndian.Uint32(entry[8:12])
			lbaSize := binary.LittleEndian.Uint32(entry[12:16])
			if lbaStart > 0 && lbaSize > 0 {
				validPartitions++
			}
		}
	}

	// If we have at least one valid partition, it's likely an MBR
	// Also check that it doesn't look like a FAT BPB
	if validPartitions > 0 {
		// FAT has specific values at certain offsets
		// Check if bytes 11-12 look like bytes per sector (usually 512, 1024, 2048, 4096)
		bps := binary.LittleEndian.Uint16(header[11:13])
		if bps == 512 || bps == 1024 || bps == 2048 || bps == 4096 {
			// Could be FAT, check for FAT-specific strings
			if bytes.Equal(header[54:59], []byte("FAT12")) ||
				bytes.Equal(header[54:59], []byte("FAT16")) ||
				bytes.Equal(header[82:87], []byte("FAT32")) {
				return false // It's FAT
			}
			// Check sectors per cluster (byte 13) - valid values are 1,2,4,8,16,32,64,128
			spc := header[13]
			if spc == 1 || spc == 2 || spc == 4 || spc == 8 || spc == 16 || spc == 32 || spc == 64 || spc == 128 {
				// Likely FAT without explicit label
				return false
			}
		}
		return true
	}

	return false
}

// isKnownPartitionType returns true if the partition type is recognized
func isKnownPartitionType(t byte) bool {
	switch t {
	case 0x01, 0x04, 0x06, 0x0B, 0x0C, 0x0E: // FAT variants
		return true
	case 0x07: // NTFS/exFAT/HPFS
		return true
	case 0x0F, 0x05: // Extended partitions
		return true
	case 0x82: // Linux swap
		return true
	case 0x83: // Linux native
		return true
	case 0x8E: // Linux LVM
		return true
	case 0xEE: // GPT protective MBR
		return true
	case 0xEF: // EFI System Partition
		return true
	case 0xFD: // Linux RAID
		return true
	default:
		return t >= 0x80 // Most values >= 0x80 are valid
	}
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
