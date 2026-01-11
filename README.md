# rawhide

A command-line tool to read files from filesystem images (FAT12/16/32, NTFS, ext2/3/4) without mounting. 
Supports MBR and GPT partition tables, with recursive access to nested images.

## Features

- **Multi-filesystem support**: FAT12, FAT16, FAT32, NTFS, ext2, ext3, ext4
- **Partition table support**: MBR (DOS) and GPT partition tables
- **Skeleton support**: APFS, HFS+ (detection and info only)
- **XTS-AES encryption**: Read encrypted disk images (AES-128/192/256-XTS)
- **Recursive image access**: Access filesystem images within images
- **Free space analysis**: Extract and probe unallocated space
- **NBD server**: Expose any file as a Linux block device
- **Automatic detection**: Identifies filesystem types via magic bytes
- **io/fs.FS compatible**: All filesystem implementations satisfy the standard Go `io/fs.FS` interface
- **Read-only**: Safe operation that never modifies the source image (unless -rw flag used)
- **No root required**: Works without mounting or special privileges

## Installation

```bash
go install github.com/lvdlvd/rawhide@latest
```

Or build from source:

```bash
git clone https://github.com/lvdlvd/rawhide
cd rawhide
go build
```

## Usage

```
rawhide [-K key] [-sz size] <image> [command] [args...]
```

If no command is given, shows filesystem information.

### Encryption Options

rawhide supports XTS-AES encryption for reading encrypted disk images:

- `-K <hex>` - XTS-AES key in hexadecimal (32, 48, or 64 bytes for AES-128/192/256)
- `-sz <size>` - Sector size for encryption (default: 512)

These flags apply to the image immediately following them and can be used at the top level or with `fscat` subcommand for nested encrypted images.

```bash
# Read encrypted disk image
rawhide -K 000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f encrypted.img ls

# Encrypted partition inside unencrypted disk
rawhide disk.img fscat -K <hex-key> p0 ls

# With custom sector size
rawhide -K <hex-key> -sz 4096 encrypted.img ls
```

### Commands

#### Default (no command) - Show filesystem info

```bash
rawhide disk.img
```

Output:
```
Filesystem: GPT

Partitions: 2

NAME   TYPE                       START         SIZE LABEL
p0     EFI System                  2048        16.0M EFI System
p1     Apple APFS                409640         1.8T 
```

#### `ls` - List directory contents

```bash
# List root directory
rawhide disk.img ls

# List with long format (permissions, size, date)
rawhide disk.img ls -l

# List a subdirectory
rawhide disk.img ls path/to/directory

# Show file info
rawhide disk.img ls -l somefile.txt
```

#### `cat` - Output file contents

```bash
# Print file to stdout
rawhide disk.img cat path/to/file.txt

# Extract to a file
rawhide disk.img cat path/to/file.txt > extracted.txt

# Dump raw partition bytes
rawhide disk.img cat p0 > partition.bin
```

#### `fscat` (alias: `fs`) - Recurse into nested image

```bash
# Access filesystem inside a partition
rawhide disk.img fscat p0 ls

# Using the short alias
rawhide disk.img fs p0 ls

# Access image file inside a filesystem
rawhide disk.img fscat p0 fscat backup.img ls

# Deep nesting with mixed aliases
rawhide outer.img fs p0 fscat inner.img cat readme.txt
```

#### `freecat` (alias: `fc`) - Output free space

Concatenates all free/unallocated space and outputs to stdout:

```bash
rawhide disk.img freecat > freespace.bin

# Using the short alias
rawhide disk.img fc > freespace.bin
```

#### `freefscat` (alias: `ffs`) - Probe free space for filesystem

Treats free space as a virtual image and attempts to detect/access a filesystem:

```bash
rawhide disk.img freefscat ls

# Using the short alias
rawhide disk.img ffs ls
```

Useful for forensics when a filesystem has been deleted but data remains.

#### `nbd` - Expose file as NBD block device

Exposes any accessible file as a Linux Network Block Device:

```bash
# Expose a partition as a read-only block device
rawhide disk.img nbd p0

# Enable read-write access
rawhide disk.img nbd -rw p0

# With custom socket path and export name
rawhide disk.img nbd -socket /tmp/my.sock -name myexport p0

# Then connect from another terminal:
sudo nbd-client -N myexport -unix /tmp/my.sock /dev/nbd0
sudo mount /dev/nbd0 /mnt
```

This allows you to mount nested images or partitions without extracting them first.

#### `freenbd` (alias: `fnbd`) - Expose free space as NBD block device

Exposes concatenated free space as a block device:

```bash
# Read-only (default)
rawhide disk.img freenbd -socket /tmp/free.sock

# Using the short alias
rawhide disk.img fnbd -socket /tmp/free.sock

# Read-write (allows writing to free space for forensic recovery)
rawhide disk.img freenbd -rw -socket /tmp/free.sock

# Connect and scan for deleted data
sudo nbd-client -N freespace -unix /tmp/free.sock /dev/nbd0
sudo photorec /dev/nbd0
```

## Examples

### Working with partitioned disks

```bash
# List partitions
rawhide disk.img ls
# Output:
# p0
# p1

# Get partition info
rawhide disk.img ls -l
# Output:
# -r--r--r--     16777216 Jan  1 00:00 p0
# -r--r--r--     32505856 Jan  1 00:00 p1

# Access filesystem in partition 0
rawhide disk.img fscat p0 ls

# Extract file from partition 1 (using short alias)
rawhide disk.img fs p1 cat documents/report.pdf > report.pdf
```

### Nested images

```bash
# Backup image stored on external drive
rawhide /dev/sdb fscat p0 fscat backups/old-system.img ls

# VM disk image inside a filesystem
rawhide nas-share.img fscat p0 fscat vms/windows.vhd ls
```

### Forensics

```bash
# Extract deleted filesystem from free space (using short alias)
rawhide evidence.img ffs ls

# Dump free space for analysis (using short alias)
rawhide evidence.img fc | strings > strings.txt
```

## Supported Formats

### Partition Tables
- MBR (Master Boot Record)
- GPT (GUID Partition Table)

### Filesystems (full support)
- FAT12, FAT16, FAT32
- NTFS
- ext2, ext3, ext4

### Filesystems (detection only)
- APFS (shows container info)
- HFS+ (shows volume info)

## Architecture

```
rawhide
├── detect/      - Filesystem type detection
├── fsys/        - Filesystem interface and implementations
│   ├── apfs/    - Apple APFS (skeleton)
│   ├── ext/     - ext2/3/4
│   ├── fat/     - FAT12/16/32
│   ├── hfsplus/ - Apple HFS+ (skeleton)
│   ├── ntfs/    - NTFS
│   └── part/    - Partition tables (MBR/GPT)
├── nbd/         - NBD (Network Block Device) server
├── xts/         - XTS-AES encryption/decryption
└── main.go      - CLI
```

