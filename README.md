# fscat

A command-line tool to read files from filesystem images (FAT12/16/32, NTFS, ext2/3/4) without mounting. Also supports MBR and GPT partition tables, allowing direct access to files within partitioned disk images.

## Features

- **Multi-filesystem support**: FAT12, FAT16, FAT32, NTFS, ext2, ext3, ext4
- **Partition table support**: MBR (DOS) and GPT partition tables
- **Nested image access**: Access filesystem images stored within other images
- **Automatic detection**: Identifies filesystem and partition table types via magic bytes
- **Transparent partition access**: Navigate through partitions using paths like `p0/path/to/file`
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
fscat <image> <command> [options] [path]
```

### Commands

#### `ls` - List directory contents

```bash
# List root directory
fscat disk.img ls

# List with long format (permissions, size, date)
fscat disk.img ls -l

# List a subdirectory
fscat disk.img ls path/to/directory

# Show all files including system files (NTFS $MFT etc)
fscat disk.img ls -a
```

#### `cat` - Output file contents

```bash
# Print file to stdout
fscat disk.img cat path/to/file.txt

# Copy to a file
fscat disk.img cat path/to/file.txt > extracted.txt

# Works with binary files
fscat disk.img cat images/photo.jpg > photo.jpg
```

#### `stat` - Show file/directory information

```bash
fscat disk.img stat path/to/file
```

#### `info` - Show filesystem information

```bash
fscat disk.img info
```

For partitioned disks, this shows partition table details:

```
Filesystem type: GPT
Detected as: GPT

Partitions: 2

NAME   TYPE                START         SIZE FSTYPE               LABEL
p0     EFI System           2048        16.0M FAT32                EFI System
p1     Basic Data          34816        31.0M ext4                 Data Partition
```

#### `free` - List free space ranges

```bash
fscat disk.img free
```

Output shows byte ranges as `[start, end)` where start is inclusive and end is exclusive:

```
Free ranges (4 ranges, 29.5M total):
[12288, 16384) 4.0K
[94208, 4206592) 3.9M
[4624384, 16773120) 11.6M
[18874368, 33550336) 14.0M
```

This is useful for:
- Finding unallocated space in filesystem images
- Data recovery and forensics
- Understanding filesystem fragmentation
- Partition table gap analysis

#### `fscat` - Access nested filesystem images

The `fscat` subcommand allows you to recursively access filesystem images stored within other images, without extracting them first:

```bash
# Access a filesystem image stored inside another image
fscat outer.img fscat path/to/inner.img ls

# Read a file from a nested image
fscat outer.img fscat inner.img cat somefile.txt

# Triple nesting through a partition table
fscat disk.img fscat p0/backup.img fscat archive.img cat data.txt
```

This works efficiently using extent mapping - the inner image data is read directly from the outer image without loading the entire inner image into memory. You can nest arbitrarily deep:

```bash
# Deep nesting example
fscat level0.img fscat level1.img fscat level2.img fscat level3.img ls
```

## Working with Partitioned Disks

Partition tables (MBR and GPT) are treated as an outer quasi-filesystem where partitions appear as directories:

```bash
# List partitions in a disk image
fscat disk.img ls
# Output:
# p0/
# p1/
# p2/

# List files in partition 0
fscat disk.img ls p0

# Extract a file from partition 1
fscat disk.img cat p1/home/user/document.txt > document.txt

# Show info for the whole disk (lists partitions)
fscat disk.img info

# Get details about a specific partition's filesystem
fscat disk.img ls -l p0
```

Paths are hierarchical: the partition name (p0, p1, etc.) comes first, followed by the path within that partition's filesystem.

## Examples

```bash
# Extract a specific file from a FAT32 USB image
fscat usb.img cat Documents/report.pdf > report.pdf

# List files on an ext4 partition image
fscat linux-root.img ls -l home/user

# Check what type of filesystem an image contains
fscat unknown.img info
```

## Supported Formats

### Partition Tables
- **MBR (DOS)**: Up to 4 primary partitions, partition type detection
- **GPT**: Full GUID partition support, partition labels, up to 128 entries
- **Free space**: Gaps between partitions and unpartitioned areas

### FAT (FAT12, FAT16, FAT32)
- Full directory traversal
- Long filename (LFN) support
- Correct timestamp parsing
- Free space from FAT cluster entries

### NTFS
- MFT parsing
- Index allocation (B-tree directories)
- Data runs and non-resident attributes
- Large file support
- Free space from $Bitmap file

### ext2/3/4
- Inode-based access
- Extent trees (ext4)
- Indirect blocks
- Directory entries with file types
- Free space from block group bitmaps

## Architecture

The project uses a layered architecture:

```
main.go          - CLI entry point
├── detect/      - Filesystem and partition table type detection
├── cmd/         - Command implementations (ls, cat, stat)
└── fsys/        - Filesystem implementations
    ├── fsys.go  - Common interface (extends io/fs.FS)
    ├── part/    - Partition tables (MBR, GPT)
    ├── fat/     - FAT12/16/32
    ├── ntfs/    - NTFS
    └── ext/     - ext2/3/4
```

All filesystem implementations satisfy the `fsys.FS` interface, which extends `io/fs.FS`:

```go
type FS interface {
    fs.FS
    fs.ReadDirFS
    fs.StatFS
    Type() string
    Close() error
}
```

The partition table implementation creates a virtual filesystem where partitions appear as directories. When you access a path within a partition, it automatically detects and opens the appropriate filesystem (FAT, NTFS, ext) within that partition's boundaries.

## Limitations

- Read-only (by design)
- No encryption support (BitLocker, LUKS, etc.)
- No compression support (NTFS compression, squashfs, etc.)
- No sparse file handling for output
- Large files are loaded into memory
- No extended/logical MBR partitions (primary partitions only)
- No nested partition tables

## License

MIT
