# fscat

A command-line tool to read files from filesystem images (FAT12/16/32, NTFS, ext2/3/4) without mounting.

## Features

- **Multi-filesystem support**: FAT12, FAT16, FAT32, NTFS, ext2, ext3, ext4
- **Automatic detection**: Identifies filesystem type via magic bytes
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

## Examples

```bash
# Extract a specific file from a FAT32 USB image
fscat usb.img cat Documents/report.pdf > report.pdf

# List files on an ext4 partition image
fscat linux-root.img ls -l home/user

# Check what type of filesystem an image contains
fscat unknown.img info
```

## Supported Filesystem Details

### FAT (FAT12, FAT16, FAT32)
- Full directory traversal
- Long filename (LFN) support
- Correct timestamp parsing

### NTFS
- MFT parsing
- Index allocation (B-tree directories)
- Data runs and non-resident attributes
- Large file support

### ext2/3/4
- Inode-based access
- Extent trees (ext4)
- Indirect blocks
- Directory entries with file types

## Architecture

The project uses a layered architecture:

```
main.go          - CLI entry point
├── detect/      - Filesystem type detection
├── cmd/         - Command implementations (ls, cat, stat)
└── fsys/        - Filesystem implementations
    ├── fsys.go  - Common interface (extends io/fs.FS)
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

## Limitations

- Read-only (by design)
- No encryption support (BitLocker, LUKS, etc.)
- No compression support (NTFS compression, squashfs, etc.)
- No sparse file handling for output
- Large files are loaded into memory

## License

MIT
