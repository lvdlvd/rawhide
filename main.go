// fscat - Read files from filesystem images (FAT, NTFS, ext2/3/4)
//
// Usage:
//
//	fscat <image> ls [-l] [path]
//	fscat <image> cat <path>
//	fscat <image> stat <path>
//	fscat <image> info
//	fscat <image> free
//	fscat <image> fscat <path/to/inner.img> <command> [args...]
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/luuk/fscat/cmd"
	"github.com/luuk/fscat/detect"
	"github.com/luuk/fscat/fsys"
	"github.com/luuk/fscat/fsys/ext"
	"github.com/luuk/fscat/fsys/fat"
	"github.com/luuk/fscat/fsys/ntfs"
	"github.com/luuk/fscat/fsys/part"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "fscat: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: fscat <image> <command> [options] [path]")
	}

	imagePath := args[0]
	cmdArgs := args[1:]

	// Open image file
	file, err := os.Open(imagePath)
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat image: %w", err)
	}

	// Detect filesystem type
	fsType, err := detect.Detect(file)
	if err != nil {
		return fmt.Errorf("detecting filesystem: %w", err)
	}

	if fsType == detect.Unknown {
		return fmt.Errorf("unknown or unsupported filesystem")
	}

	// Open filesystem
	filesystem, err := openFilesystem(file, info.Size(), fsType)
	if err != nil {
		return fmt.Errorf("opening filesystem: %w", err)
	}
	defer filesystem.Close()

	return runCommand(filesystem, cmdArgs, stdout, stderr)
}

// runCommand executes a command against a filesystem, supporting recursive fscat
func runCommand(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("missing command")
	}

	command := args[0]
	cmdArgs := args[1:]

	switch command {
	case "ls":
		return runLs(filesystem, cmdArgs, stdout)
	case "cat":
		return runCat(filesystem, cmdArgs, stdout)
	case "stat":
		return runStat(filesystem, cmdArgs, stdout)
	case "info":
		return runInfo(filesystem, stdout)
	case "free":
		return runFree(filesystem, stdout)
	case "fscat":
		return runNestedFscat(filesystem, cmdArgs, stdout, stderr)
	default:
		return fmt.Errorf("unknown command: %s (use ls, cat, stat, info, free, or fscat)", command)
	}
}

// runNestedFscat handles the fscat subcommand for nested images
func runNestedFscat(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: fscat <path/to/inner.img> <command> [args...]")
	}

	innerPath := args[0]
	remainingArgs := args[1:]

	// Get file info to check it's not a directory and get size
	info, err := filesystem.Stat(innerPath)
	if err != nil {
		return fmt.Errorf("stat inner image %s: %w", innerPath, err)
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not an image file", innerPath)
	}

	fileSize := info.Size()

	// Try to get extents for zero-copy access
	var reader io.ReaderAt

	if em, ok := filesystem.(fsys.ExtentMapper); ok {
		extents, err := em.FileExtents(innerPath)
		if err == nil && len(extents) > 0 {
			// Get the underlying ReaderAt from the filesystem
			// We need to access the base reader - check if filesystem exposes it
			if baseReader := getBaseReader(filesystem); baseReader != nil {
				reader = fsys.NewExtentReaderAt(baseReader, extents, fileSize)
			}
		}
	}

	// Fall back to reading into memory if extent mapping didn't work
	if reader == nil {
		file, err := filesystem.Open(innerPath)
		if err != nil {
			return fmt.Errorf("opening inner image %s: %w", innerPath, err)
		}
		defer file.Close()

		data, err := io.ReadAll(file.(io.Reader))
		if err != nil {
			return fmt.Errorf("reading inner image %s: %w", innerPath, err)
		}
		reader = bytes.NewReader(data)
		fileSize = int64(len(data))
	}

	// Detect filesystem type
	fsType, err := detect.Detect(reader)
	if err != nil {
		return fmt.Errorf("detecting filesystem in %s: %w", innerPath, err)
	}

	if fsType == detect.Unknown {
		return fmt.Errorf("unknown or unsupported filesystem in %s", innerPath)
	}

	// Open the inner filesystem
	innerFS, err := openFilesystem(reader, fileSize, fsType)
	if err != nil {
		return fmt.Errorf("opening filesystem in %s: %w", innerPath, err)
	}
	defer innerFS.Close()

	// Recursively execute the command
	return runCommand(innerFS, remainingArgs, stdout, stderr)
}

// ReaderAtExposer is an optional interface for filesystems that can expose their base reader
type ReaderAtExposer interface {
	BaseReader() io.ReaderAt
}

// getBaseReader attempts to get the underlying ReaderAt from a filesystem
func getBaseReader(filesystem fsys.FS) io.ReaderAt {
	if exp, ok := filesystem.(ReaderAtExposer); ok {
		return exp.BaseReader()
	}
	return nil
}

func openFilesystem(r io.ReaderAt, size int64, fsType detect.Type) (fsys.FS, error) {
	switch {
	case fsType.IsPartitionTable():
		return part.Open(r, size, fsType)
	case fsType.IsFAT():
		return fat.Open(r, size)
	case fsType.IsExt():
		return ext.Open(r, size)
	case fsType == detect.NTFS:
		return ntfs.Open(r, size)
	default:
		return nil, fmt.Errorf("unsupported filesystem type: %s", fsType)
	}
}

func runLs(filesystem fsys.FS, args []string, out io.Writer) error {
	flagSet := flag.NewFlagSet("ls", flag.ContinueOnError)
	long := flagSet.Bool("l", false, "use long listing format")
	all := flagSet.Bool("a", false, "show all files including system files")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	path := "."
	if flagSet.NArg() > 0 {
		path = flagSet.Arg(0)
	}

	return cmd.Ls(filesystem, path, out, cmd.LsOptions{
		Long: *long,
		All:  *all,
	})
}

func runCat(filesystem fsys.FS, args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("cat requires a path argument")
	}

	return cmd.Cat(filesystem, args[0], out)
}

func runStat(filesystem fsys.FS, args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("stat requires a path argument")
	}

	return cmd.Stat(filesystem, args[0], out)
}

func runInfo(filesystem fsys.FS, out io.Writer) error {
	fmt.Fprintf(out, "Filesystem type: %s\n", filesystem.Type())

	// Show partition information if this is a partition table
	if pfs, ok := filesystem.(*part.FS); ok {
		partitions := pfs.Partitions()
		fmt.Fprintf(out, "\nPartitions: %d\n", len(partitions))
		fmt.Fprintf(out, "\n%-6s %-12s %12s %12s %-20s %s\n",
			"NAME", "TYPE", "START", "SIZE", "FSTYPE", "LABEL")

		for _, p := range partitions {
			typeStr := part.PartitionTypeString(p)
			detectedType, _ := pfs.DetectPartitionFS(p)
			label := p.Label
			if label == "" && p.Bootable {
				label = "(bootable)"
			}
			fmt.Fprintf(out, "%-6s %-12s %12d %12s %-20s %s\n",
				p.Name,
				typeStr,
				p.StartLBA,
				formatSize(p.SizeBytes()),
				detectedType,
				label)
		}
	}

	return nil
}

func runFree(filesystem fsys.FS, out io.Writer) error {
	fb, ok := filesystem.(fsys.FreeBlocker)
	if !ok {
		return fmt.Errorf("filesystem type %s does not support free block listing", filesystem.Type())
	}

	ranges, err := fb.FreeBlocks()
	if err != nil {
		return fmt.Errorf("getting free blocks: %w", err)
	}

	// Calculate total free space
	var totalFree int64
	for _, r := range ranges {
		totalFree += r.Size()
	}

	fmt.Fprintf(out, "Free ranges (%d ranges, %s total):\n", len(ranges), formatSize(totalFree))
	for _, r := range ranges {
		fmt.Fprintf(out, "[%d, %d) %s\n", r.Start, r.End, formatSize(r.Size()))
	}

	return nil
}

func formatSize(bytes int64) string {
	const (
		KB = 1024
		MB = KB * 1024
		GB = MB * 1024
		TB = GB * 1024
	)

	switch {
	case bytes >= TB:
		return fmt.Sprintf("%.1fT", float64(bytes)/TB)
	case bytes >= GB:
		return fmt.Sprintf("%.1fG", float64(bytes)/GB)
	case bytes >= MB:
		return fmt.Sprintf("%.1fM", float64(bytes)/MB)
	case bytes >= KB:
		return fmt.Sprintf("%.1fK", float64(bytes)/KB)
	default:
		return fmt.Sprintf("%dB", bytes)
	}
}
