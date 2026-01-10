//go:build ignore

package main

import (
	"encoding/binary"
	"fmt"
	"os"
	"os/exec"
)

func main() {
	if err := createMBRDisk(); err != nil {
		fmt.Fprintf(os.Stderr, "MBR: %v\n", err)
	}
	if err := createGPTDisk(); err != nil {
		fmt.Fprintf(os.Stderr, "GPT: %v\n", err)
	}
}

func createMBRDisk() error {
	const diskSize = 64 * 1024 * 1024 // 64MB
	const sectorSize = 512

	// Create empty disk image
	f, err := os.Create("testdata/mbr-disk.img")
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(diskSize); err != nil {
		return err
	}

	// Write MBR
	mbr := make([]byte, 512)

	// Partition 1: FAT32, starts at sector 2048, 32MB
	p1Start := uint32(2048)
	p1Size := uint32(32 * 1024 * 1024 / sectorSize)
	writePartEntry(mbr[446:462], 0x00, 0x0C, p1Start, p1Size) // FAT32 LBA

	// Partition 2: Linux, after partition 1, ~30MB
	p2Start := p1Start + p1Size
	p2Size := uint32(30 * 1024 * 1024 / sectorSize)
	writePartEntry(mbr[462:478], 0x00, 0x83, p2Start, p2Size) // Linux

	// Boot signature
	mbr[510] = 0x55
	mbr[511] = 0xAA

	if _, err := f.WriteAt(mbr, 0); err != nil {
		return err
	}

	// Format partition 1 as FAT32
	p1Offset := int64(p1Start) * sectorSize
	if err := formatFAT32(f, p1Offset, int64(p1Size)*sectorSize); err != nil {
		return fmt.Errorf("format FAT32: %w", err)
	}

	// Format partition 2 as ext4 using mkfs.ext4 on loop device
	p2Offset := int64(p2Start) * sectorSize
	if err := formatExt4(f, p2Offset, int64(p2Size)*sectorSize); err != nil {
		fmt.Printf("Note: ext4 format skipped: %v\n", err)
	}

	fmt.Println("Created mbr-disk.img")
	return nil
}

func writePartEntry(entry []byte, boot, ptype byte, startLBA, sizeLBA uint32) {
	entry[0] = boot
	entry[1] = 0 // CHS start (unused)
	entry[2] = 0
	entry[3] = 0
	entry[4] = ptype
	entry[5] = 0 // CHS end (unused)
	entry[6] = 0
	entry[7] = 0
	binary.LittleEndian.PutUint32(entry[8:12], startLBA)
	binary.LittleEndian.PutUint32(entry[12:16], sizeLBA)
}

func formatFAT32(f *os.File, offset, size int64) error {
	// Create a minimal FAT32 filesystem
	const sectorSize = 512
	const sectorsPerCluster = 8
	const reservedSectors = 32
	const numFATs = 2

	totalSectors := uint32(size / sectorSize)
	
	// Calculate FAT size
	// Each FAT entry is 4 bytes, each cluster is sectorsPerCluster sectors
	dataSectors := totalSectors - reservedSectors
	numClusters := dataSectors / sectorsPerCluster
	fatSectors := (numClusters * 4 + sectorSize - 1) / sectorSize
	
	// BPB (BIOS Parameter Block)
	bpb := make([]byte, sectorSize)
	copy(bpb[0:3], []byte{0xEB, 0x58, 0x90}) // Jump instruction
	copy(bpb[3:11], []byte("MSDOS5.0"))      // OEM name
	
	binary.LittleEndian.PutUint16(bpb[11:13], sectorSize)
	bpb[13] = sectorsPerCluster
	binary.LittleEndian.PutUint16(bpb[14:16], reservedSectors)
	bpb[16] = numFATs
	binary.LittleEndian.PutUint16(bpb[17:19], 0) // Root entries (0 for FAT32)
	binary.LittleEndian.PutUint16(bpb[19:21], 0) // Total sectors 16 (0 for FAT32)
	bpb[21] = 0xF8                               // Media type (fixed disk)
	binary.LittleEndian.PutUint16(bpb[22:24], 0) // FAT size 16 (0 for FAT32)
	binary.LittleEndian.PutUint16(bpb[24:26], 63) // Sectors per track
	binary.LittleEndian.PutUint16(bpb[26:28], 255) // Heads
	binary.LittleEndian.PutUint32(bpb[28:32], uint32(offset/sectorSize)) // Hidden sectors
	binary.LittleEndian.PutUint32(bpb[32:36], totalSectors) // Total sectors 32
	
	// FAT32 extended BPB
	binary.LittleEndian.PutUint32(bpb[36:40], fatSectors) // FAT size 32
	binary.LittleEndian.PutUint16(bpb[40:42], 0)          // Ext flags
	binary.LittleEndian.PutUint16(bpb[42:44], 0)          // FS version
	binary.LittleEndian.PutUint32(bpb[44:48], 2)          // Root cluster
	binary.LittleEndian.PutUint16(bpb[48:50], 1)          // FSInfo sector
	binary.LittleEndian.PutUint16(bpb[50:52], 6)          // Backup boot sector
	// bytes 52-63 reserved
	bpb[64] = 0x80                                        // Drive number
	bpb[66] = 0x29                                        // Extended boot signature
	binary.LittleEndian.PutUint32(bpb[67:71], 0x12345678) // Volume serial
	copy(bpb[71:82], []byte("PARTITION1 "))               // Volume label
	copy(bpb[82:90], []byte("FAT32   "))                  // FS type
	
	bpb[510] = 0x55
	bpb[511] = 0xAA
	
	if _, err := f.WriteAt(bpb, offset); err != nil {
		return err
	}
	
	// Write FAT tables
	fatOffset := offset + int64(reservedSectors)*sectorSize
	fat := make([]byte, fatSectors*sectorSize)
	
	// First entries
	binary.LittleEndian.PutUint32(fat[0:4], 0x0FFFFFF8)  // Media type
	binary.LittleEndian.PutUint32(fat[4:8], 0x0FFFFFFF)  // End of chain marker
	binary.LittleEndian.PutUint32(fat[8:12], 0x0FFFFFFF) // Root directory (cluster 2)
	
	// Write both FAT copies
	if _, err := f.WriteAt(fat, fatOffset); err != nil {
		return err
	}
	if _, err := f.WriteAt(fat, fatOffset+int64(fatSectors)*sectorSize); err != nil {
		return err
	}
	
	// Create root directory with test file
	rootCluster := reservedSectors + fatSectors*numFATs
	rootOffset := offset + int64(rootCluster)*sectorSize
	
	root := make([]byte, sectorsPerCluster*sectorSize)
	// Volume label entry
	copy(root[0:11], []byte("PARTITION1 "))
	root[11] = 0x08 // Volume label attribute
	
	// Create test file entry: HELLO.TXT
	copy(root[32:43], []byte("HELLO   TXT"))
	root[43] = 0x20 // Archive attribute
	binary.LittleEndian.PutUint16(root[58:60], 3) // First cluster low (cluster 3)
	binary.LittleEndian.PutUint32(root[60:64], 13) // File size
	
	if _, err := f.WriteAt(root, rootOffset); err != nil {
		return err
	}
	
	// Mark cluster 3 as end of chain for the file
	binary.LittleEndian.PutUint32(fat[12:16], 0x0FFFFFFF)
	if _, err := f.WriteAt(fat, fatOffset); err != nil {
		return err
	}
	if _, err := f.WriteAt(fat, fatOffset+int64(fatSectors)*sectorSize); err != nil {
		return err
	}
	
	// Write file content to cluster 3
	fileClusterOffset := rootOffset + int64(sectorsPerCluster)*sectorSize
	if _, err := f.WriteAt([]byte("Hello, MBR!\n\x00"), fileClusterOffset); err != nil {
		return err
	}
	
	return nil
}

func formatExt4(f *os.File, offset, size int64) error {
	// Try to use mkfs.ext4 with loop device
	// This is best-effort since it requires root
	cmd := exec.Command("mkfs.ext4", "-q", "-F", "-E", fmt.Sprintf("offset=%d", offset), f.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func createGPTDisk() error {
	const diskSize = 64 * 1024 * 1024 // 64MB
	const sectorSize = 512

	f, err := os.Create("testdata/gpt-disk.img")
	if err != nil {
		return err
	}
	defer f.Close()

	if err := f.Truncate(diskSize); err != nil {
		return err
	}

	totalSectors := diskSize / sectorSize

	// Write protective MBR
	mbr := make([]byte, 512)
	writePartEntry(mbr[446:462], 0x00, 0xEE, 1, uint32(totalSectors-1)) // GPT protective
	mbr[510] = 0x55
	mbr[511] = 0xAA
	if _, err := f.WriteAt(mbr, 0); err != nil {
		return err
	}

	// GPT Header at LBA 1
	gptHeader := make([]byte, sectorSize)
	copy(gptHeader[0:8], []byte("EFI PART"))
	binary.LittleEndian.PutUint32(gptHeader[8:12], 0x00010000)    // Revision 1.0
	binary.LittleEndian.PutUint32(gptHeader[12:16], 92)           // Header size
	// CRC32 at 16:20 - skip for simplicity
	// Reserved at 20:24
	binary.LittleEndian.PutUint64(gptHeader[24:32], 1)            // My LBA
	binary.LittleEndian.PutUint64(gptHeader[32:40], uint64(totalSectors-1)) // Alternate LBA
	binary.LittleEndian.PutUint64(gptHeader[40:48], 34)           // First usable LBA
	binary.LittleEndian.PutUint64(gptHeader[48:56], uint64(totalSectors-34)) // Last usable LBA
	// Disk GUID at 56:72
	copy(gptHeader[56:72], []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08, 0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10})
	binary.LittleEndian.PutUint64(gptHeader[72:80], 2)            // Partition entry LBA
	binary.LittleEndian.PutUint32(gptHeader[80:84], 128)          // Number of partition entries
	binary.LittleEndian.PutUint32(gptHeader[84:88], 128)          // Size of partition entry

	if _, err := f.WriteAt(gptHeader, sectorSize); err != nil {
		return err
	}

	// GPT Partition entries starting at LBA 2
	entries := make([]byte, 128*128) // 128 entries * 128 bytes

	// Partition 1: EFI System Partition (FAT32)
	p1 := entries[0:128]
	// EFI System GUID: C12A7328-F81F-11D2-BA4B-00A0C93EC93B
	copy(p1[0:16], []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B})
	// Unique GUID
	copy(p1[16:32], []byte{0xA1, 0xB2, 0xC3, 0xD4, 0xE5, 0xF6, 0x07, 0x18, 0x29, 0x3A, 0x4B, 0x5C, 0x6D, 0x7E, 0x8F, 0x90})
	binary.LittleEndian.PutUint64(p1[32:40], 2048)  // Start LBA
	binary.LittleEndian.PutUint64(p1[40:48], 34815) // End LBA (~16MB)
	// Attributes at 48:56
	// Name at 56:128 (UTF-16LE)
	name1 := utf16Encode("EFI System")
	copy(p1[56:], name1)

	// Partition 2: Basic Data (for Linux or whatever)
	p2 := entries[128:256]
	// Basic Data GUID: EBD0A0A2-B9E5-4433-87C0-68B6B72699C7
	copy(p2[0:16], []byte{0xA2, 0xA0, 0xD0, 0xEB, 0xE5, 0xB9, 0x33, 0x44, 0x87, 0xC0, 0x68, 0xB6, 0xB7, 0x26, 0x99, 0xC7})
	copy(p2[16:32], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00})
	binary.LittleEndian.PutUint64(p2[32:40], 34816) // Start LBA
	binary.LittleEndian.PutUint64(p2[40:48], 98303) // End LBA (~31MB)
	name2 := utf16Encode("Data Partition")
	copy(p2[56:], name2)

	if _, err := f.WriteAt(entries, 2*sectorSize); err != nil {
		return err
	}

	// Format partition 1 as FAT32
	if err := formatFAT32(f, 2048*sectorSize, (34815-2048+1)*sectorSize); err != nil {
		return fmt.Errorf("format FAT32: %w", err)
	}

	fmt.Println("Created gpt-disk.img")
	return nil
}

func utf16Encode(s string) []byte {
	runes := []rune(s)
	buf := make([]byte, len(runes)*2)
	for i, r := range runes {
		binary.LittleEndian.PutUint16(buf[i*2:], uint16(r))
	}
	return buf
}
