# fscat

A command-line tool to read files from filesystem images (FAT12/16/32, NTFS, ext2/3/4) without mounting. Also supports MBR and GPT partition tables, with recursive access to nested images.

## Features

- **Multi-filesystem support**: FAT12, FAT16, FAT32, NTFS, ext2, ext3, ext4
- **Partition table support**: MBR (DOS) and GPT partition tables
- **Skeleton support**: APFS, HFS+ (detection and info only)
- **Recursive image access**: Access filesystem images within images
- **Free space analysis**: Extract and probe unallocated space
- **NBD server**: Expose any file as a Linux block device
- **Automatic detection**: Identifies filesystem types via magic bytes
- **io/fs.FS compatible**: All filesystem implementations satisfy the standard Go `io/fs.FS` interface
- **Read-only**: Safe operation that never modifies the source image
- **No root required**: Works without mounting or special privileges

## Installation

```bash
go install github.com/luuk/fscat@latest
```

Or build from source:

```bash
git clone https://github.com/luuk/fscat
cd fscat
go build
```

## Usage

```
fscat <image> [command] [args...]
```

If no command is given, shows filesystem information.

### Commands

#### Default (no command) - Show filesystem info

```bash
fscat disk.img
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
fscat disk.img ls

# List with long format (permissions, size, date)
fscat disk.img ls -l

# List a subdirectory
fscat disk.img ls path/to/directory

# Show file info
fscat disk.img ls -l somefile.txt
```

#### `cat` - Output file contents

```bash
# Print file to stdout
fscat disk.img cat path/to/file.txt

# Extract to a file
fscat disk.img cat path/to/file.txt > extracted.txt

# Dump raw partition bytes
fscat disk.img cat p0 > partition.bin
```

#### `fscat` - Recurse into nested image

```bash
# Access filesystem inside a partition
fscat disk.img fscat p0 ls

# Access image file inside a filesystem
fscat disk.img fscat p0 fscat backup.img ls

# Deep nesting
fscat outer.img fscat p0 fscat inner.img cat readme.txt
```

#### `freecat` - Output free space

Concatenates all free/unallocated space and outputs to stdout:

```bash
fscat disk.img freecat > freespace.bin
```

#### `freefscat` - Probe free space for filesystem

Treats free space as a virtual image and attempts to detect/access a filesystem:

```bash
fscat disk.img freefscat ls
```

Useful for forensics when a filesystem has been deleted but data remains.

#### `nbd` - Expose file as NBD block device

Exposes any accessible file as a Linux Network Block Device:

```bash
# Expose a partition as a block device
fscat disk.img nbd p0

# With custom socket path and export name
fscat disk.img nbd -socket /tmp/my.sock -name myexport p0

# Then connect from another terminal:
sudo nbd-client -N myexport -unix /tmp/my.sock /dev/nbd0
sudo mount /dev/nbd0 /mnt
```

This allows you to mount nested images or partitions without extracting them first.

#### `freenbd` - Expose free space as NBD block device

Exposes concatenated free space as a block device:

```bash
fscat disk.img freenbd -socket /tmp/free.sock

# Connect and scan for deleted data
sudo nbd-client -N freespace -unix /tmp/free.sock /dev/nbd0
sudo photorec /dev/nbd0
```

## Examples

### Working with partitioned disks

```bash
# List partitions
fscat disk.img ls
# Output:
# p0
# p1

# Get partition info
fscat disk.img ls -l
# Output:
# -r--r--r--     16777216 Jan  1 00:00 p0
# -r--r--r--     32505856 Jan  1 00:00 p1

# Access filesystem in partition 0
fscat disk.img fscat p0 ls

# Extract file from partition 1
fscat disk.img fscat p1 cat documents/report.pdf > report.pdf
```

### Nested images

```bash
# Backup image stored on external drive
fscat /dev/sdb fscat p0 fscat backups/old-system.img ls

# VM disk image inside a filesystem
fscat nas-share.img fscat p0 fscat vms/windows.vhd ls
```

### Forensics

```bash
# Extract deleted filesystem from free space
fscat evidence.img freefscat ls

# Dump free space for analysis
fscat evidence.img freecat | strings > strings.txt
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
fscat
├── detect/      - Filesystem type detection
├── fsys/        - Filesystem interface and implementations
│   ├── apfs/    - Apple APFS (skeleton)
│   ├── ext/     - ext2/3/4
│   ├── fat/     - FAT12/16/32
│   ├── hfsplus/ - Apple HFS+ (skeleton)
│   ├── ntfs/    - NTFS
│   └── part/    - Partition tables (MBR/GPT)
├── nbd/         - NBD (Network Block Device) server
└── main.go      - CLI
```

## License

MIT
