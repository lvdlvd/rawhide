package cmd

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/luuk/fscat/fsys"
)

// Cat copies the contents of a file to the given writer.
func Cat(filesystem fsys.FS, fsPath string, out io.Writer) error {
	// Normalize path
	fsPath = normalizePath(fsPath)

	file, err := filesystem.Open(fsPath)
	if err != nil {
		return err
	}
	defer file.Close()

	// Check if it's a directory
	info, err := file.Stat()
	if err != nil {
		return err
	}

	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", fsPath)
	}

	// Check if we can read
	reader, ok := file.(io.Reader)
	if !ok {
		return fmt.Errorf("%s: cannot read file", fsPath)
	}

	_, err = io.Copy(out, reader)
	return err
}

// Stat shows detailed information about a file or directory.
func Stat(filesystem fsys.FS, fsPath string, out io.Writer) error {
	fsPath = normalizePath(fsPath)

	info, err := fs.Stat(filesystem, fsPath)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "  File: %s\n", info.Name())
	fmt.Fprintf(out, "  Size: %d\n", info.Size())
	fmt.Fprintf(out, "  Mode: %s\n", info.Mode())
	fmt.Fprintf(out, "ModTime: %s\n", info.ModTime())

	if fi, ok := info.(fsys.FileInfo); ok {
		fmt.Fprintf(out, " Inode: %d\n", fi.Inode())
	}

	return nil
}
