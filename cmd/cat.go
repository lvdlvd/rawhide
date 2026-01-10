package cmd

import (
	"fmt"
	"io"
	"io/fs"

	"github.com/luuk/fscat/fsys"
)

// Cat copies the contents of a file to the given writer.
// When the filesystem supports extent mapping, it streams directly
// from the underlying image without loading the file into memory.
func Cat(filesystem fsys.FS, fsPath string, out io.Writer) error {
	// Normalize path
	fsPath = normalizePath(fsPath)

	// Check if it's a directory
	info, err := fs.Stat(filesystem, fsPath)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s: is a directory", fsPath)
	}

	fileSize := info.Size()

	// Try extent-based streaming first
	if em, ok := filesystem.(fsys.ExtentMapper); ok {
		if br, ok := filesystem.(interface{ BaseReader() io.ReaderAt }); ok {
			extents, err := em.FileExtents(fsPath)
			if err == nil && len(extents) > 0 {
				reader := fsys.NewExtentReaderAt(br.BaseReader(), extents, fileSize)
				return streamFromReaderAt(reader, fileSize, out)
			}
		}
	}

	// Fall back to standard file reading
	file, err := filesystem.Open(fsPath)
	if err != nil {
		return err
	}
	defer file.Close()

	reader, ok := file.(io.Reader)
	if !ok {
		return fmt.Errorf("%s: cannot read file", fsPath)
	}

	_, err = io.Copy(out, reader)
	return err
}

// streamFromReaderAt copies data from a ReaderAt to a Writer in chunks
func streamFromReaderAt(r io.ReaderAt, size int64, out io.Writer) error {
	const bufSize = 64 * 1024 // 64KB chunks
	buf := make([]byte, bufSize)
	offset := int64(0)

	for offset < size {
		toRead := int64(bufSize)
		if offset+toRead > size {
			toRead = size - offset
		}

		n, err := r.ReadAt(buf[:toRead], offset)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
			offset += int64(n)
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}

	return nil
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
