// fscat - Read files from filesystem images (FAT, NTFS, ext2/3/4)
//
// Usage:
//
//	fscat <image> ls [-l] [path]
//	fscat <image> cat <path>
//	fscat <image> stat <path>
//	fscat <image> info
package main

import (
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
	command := args[1]
	cmdArgs := args[2:]

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

	// Execute command
	switch command {
	case "ls":
		return runLs(filesystem, cmdArgs, stdout)
	case "cat":
		return runCat(filesystem, cmdArgs, stdout)
	case "stat":
		return runStat(filesystem, cmdArgs, stdout)
	case "info":
		return runInfo(filesystem, fsType, stdout)
	default:
		return fmt.Errorf("unknown command: %s (use ls, cat, stat, or info)", command)
	}
}

func openFilesystem(r io.ReaderAt, size int64, fsType detect.Type) (fsys.FS, error) {
	switch {
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
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	long := fs.Bool("l", false, "use long listing format")
	all := fs.Bool("a", false, "show all files including system files")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := "."
	if fs.NArg() > 0 {
		path = fs.Arg(0)
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

func runInfo(filesystem fsys.FS, fsType detect.Type, out io.Writer) error {
	fmt.Fprintf(out, "Filesystem type: %s\n", filesystem.Type())
	fmt.Fprintf(out, "Detected as: %s\n", fsType)
	return nil
}
