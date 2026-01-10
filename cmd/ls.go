// Package cmd implements the fscat commands.
package cmd

import (
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/luuk/fscat/fsys"
)

// LsOptions controls ls behavior
type LsOptions struct {
	Long       bool // Long format (-l)
	All        bool // Show all files including system files (-a)
}

// Ls lists the contents of a path in the filesystem.
// If the path is a file, it shows file information.
// If the path is a directory, it lists its contents.
func Ls(filesystem fsys.FS, fsPath string, out io.Writer, opts LsOptions) error {
	// Normalize path
	fsPath = normalizePath(fsPath)

	info, err := fs.Stat(filesystem, fsPath)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return listDirectory(filesystem, fsPath, out, opts)
	}

	return showFileInfo(info, out, opts.Long)
}

func normalizePath(p string) string {
	// Remove leading /
	p = strings.TrimPrefix(p, "/")
	// Handle empty path
	if p == "" {
		return "."
	}
	// Clean the path
	p = path.Clean(p)
	return p
}

func listDirectory(filesystem fsys.FS, dirPath string, out io.Writer, opts LsOptions) error {
	entries, err := fs.ReadDir(filesystem, dirPath)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		name := entry.Name()
		
		// Skip system/hidden files unless -a is specified
		if !opts.All && strings.HasPrefix(name, "$") {
			continue
		}

		if opts.Long {
			info, err := entry.Info()
			if err != nil {
				fmt.Fprintf(out, "%-10s %12s %s %s\n", "?????????", "?", "????????????", name)
				continue
			}
			printLongFormat(info, out)
		} else {
			if entry.IsDir() {
				name += "/"
			}
			fmt.Fprintln(out, name)
		}
	}

	return nil
}

func showFileInfo(info fs.FileInfo, out io.Writer, long bool) error {
	if long {
		printLongFormat(info, out)
	} else {
		fmt.Fprintln(out, info.Name())
	}
	return nil
}

func printLongFormat(info fs.FileInfo, out io.Writer) {
	mode := info.Mode()
	size := info.Size()
	modTime := info.ModTime().Format("Jan _2 15:04")
	name := info.Name()

	// Check if we have inode info
	var inode string
	if fi, ok := info.(fsys.FileInfo); ok {
		inode = fmt.Sprintf("%8d ", fi.Inode())
	} else {
		inode = ""
	}

	fmt.Fprintf(out, "%s%s %12d %s %s\n", inode, mode, size, modTime, name)
}
