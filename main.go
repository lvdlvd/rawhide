// fscat - Read files from filesystem images (FAT, NTFS, ext2/3/4, APFS, HFS+)
//
// Usage:
//
//	fscat [-K key] [-sector size] [-tweak-offset n] <image> [command] [args...]
//	fscat <image> ls [-l] [path]                  - list directory or file info
//	fscat <image> cat <path>                      - copy file to stdout
//	fscat <image> fscat [-K key] <path> [cmd]     - recurse into nested image
//	fscat <image> freecat                         - copy free space to stdout
//	fscat <image> freefscat [cmd] [args]          - probe free space as image
//	fscat <image> nbd [-rw] <path> [-socket path] - expose file as NBD block device
//	fscat <image> freenbd [-rw] [-socket path]    - expose free space as NBD device
package main

import (
	"bytes"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/luuk/fscat/detect"
	"github.com/luuk/fscat/fsys"
	"github.com/luuk/fscat/fsys/apfs"
	"github.com/luuk/fscat/fsys/ext"
	"github.com/luuk/fscat/fsys/fat"
	"github.com/luuk/fscat/fsys/hfsplus"
	"github.com/luuk/fscat/fsys/ntfs"
	"github.com/luuk/fscat/fsys/part"
	"github.com/luuk/fscat/nbd"
	"github.com/luuk/fscat/xts"
)

// cryptoParams holds encryption parameters
type cryptoParams struct {
	key         []byte
	sectorSize  int
	tweakOffset uint64
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "fscat: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: fscat [-K key] [-sector size] [-tweak-offset n] <image> [command] [args...]")
	}

	// Parse encryption flags
	flagSet := flag.NewFlagSet("fscat", flag.ContinueOnError)
	keyHex := flagSet.String("K", "", "XTS-AES key in hexadecimal")
	sectorSize := flagSet.Int("sector", 512, "Sector size for XTS encryption")
	tweakOffset := flagSet.Uint64("tweak-offset", 0, "Starting tweak value offset")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	if flagSet.NArg() < 1 {
		return fmt.Errorf("usage: fscat [-K key] [-sector size] [-tweak-offset n] <image> [command] [args...]")
	}

	imagePath := flagSet.Arg(0)
	cmdArgs := flagSet.Args()[1:]

	// Parse crypto params
	var crypto *cryptoParams
	if *keyHex != "" {
		key, err := hex.DecodeString(*keyHex)
		if err != nil {
			return fmt.Errorf("invalid key hex: %w", err)
		}
		crypto = &cryptoParams{
			key:         key,
			sectorSize:  *sectorSize,
			tweakOffset: *tweakOffset,
		}
	}

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

	// Wrap with decryption if needed
	var reader io.ReaderAt = file
	size := info.Size()
	if crypto != nil {
		reader, err = wrapWithDecryption(reader, size, crypto)
		if err != nil {
			return fmt.Errorf("setting up decryption: %w", err)
		}
	}

	// Detect filesystem type
	fsType, err := detect.Detect(reader)
	if err != nil {
		return fmt.Errorf("detecting filesystem: %w", err)
	}

	if fsType == detect.Unknown {
		return fmt.Errorf("unknown or unsupported filesystem")
	}

	// Open filesystem
	filesystem, err := openFilesystem(reader, size, fsType)
	if err != nil {
		return fmt.Errorf("opening filesystem: %w", err)
	}
	defer filesystem.Close()

	return runCommand(filesystem, cmdArgs, stdout, stderr)
}

// wrapWithDecryption wraps a reader with XTS decryption
func wrapWithDecryption(r io.ReaderAt, size int64, crypto *cryptoParams) (*xts.ReaderAt, error) {
	cipher, err := xts.New(crypto.key, crypto.sectorSize, crypto.tweakOffset)
	if err != nil {
		return nil, err
	}
	return xts.NewReaderAt(r, cipher, size), nil
}

// runCommand executes a command against a filesystem
func runCommand(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	// Default command is info
	if len(args) == 0 {
		return runInfo(filesystem, stdout)
	}

	command := args[0]
	cmdArgs := args[1:]

	switch command {
	case "ls":
		return runLs(filesystem, cmdArgs, stdout)
	case "cat":
		return runCat(filesystem, cmdArgs, stdout)
	case "fscat":
		return runFscat(filesystem, cmdArgs, stdout, stderr)
	case "freecat":
		return runFreeCat(filesystem, stdout)
	case "freefscat":
		return runFreeFscat(filesystem, cmdArgs, stdout, stderr)
	case "nbd":
		return runNbd(filesystem, cmdArgs, stdout, stderr)
	case "freenbd":
		return runFreeNbd(filesystem, cmdArgs, stdout, stderr)
	default:
		return fmt.Errorf("unknown command: %s (use ls, cat, fscat, freecat, freefscat, nbd, freenbd)", command)
	}
}

// getReaderForPath returns a ReaderAt and size for a file path using extent mapping
func getReaderForPath(filesystem fsys.FS, path string) (io.ReaderAt, int64, error) {
	info, err := filesystem.Stat(path)
	if err != nil {
		return nil, 0, err
	}
	if info.IsDir() {
		return nil, 0, fmt.Errorf("%s is a directory", path)
	}

	fileSize := info.Size()

	// Try extent-based access first
	if em, ok := filesystem.(fsys.ExtentMapper); ok {
		if br, ok := filesystem.(interface{ BaseReader() io.ReaderAt }); ok {
			extents, err := em.FileExtents(path)
			if err == nil && len(extents) > 0 {
				return fsys.NewExtentReaderAt(br.BaseReader(), extents, fileSize), fileSize, nil
			}
		}
	}

	// Fall back to reading into memory
	file, err := filesystem.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	data, err := io.ReadAll(file.(io.Reader))
	if err != nil {
		return nil, 0, err
	}
	return bytes.NewReader(data), int64(len(data)), nil
}

// runFscat handles the fscat command for nested images
func runFscat(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	// Parse encryption flags
	flagSet := flag.NewFlagSet("fscat", flag.ContinueOnError)
	keyHex := flagSet.String("K", "", "XTS-AES key in hexadecimal")
	sectorSize := flagSet.Int("sector", 512, "Sector size for XTS encryption")
	tweakOffset := flagSet.Uint64("tweak-offset", 0, "Starting tweak value offset")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	if flagSet.NArg() < 1 {
		return fmt.Errorf("fscat requires a path argument")
	}

	innerPath := flagSet.Arg(0)
	remainingArgs := flagSet.Args()[1:]

	// Parse crypto params
	var crypto *cryptoParams
	if *keyHex != "" {
		key, err := hex.DecodeString(*keyHex)
		if err != nil {
			return fmt.Errorf("invalid key hex: %w", err)
		}
		crypto = &cryptoParams{
			key:         key,
			sectorSize:  *sectorSize,
			tweakOffset: *tweakOffset,
		}
	}

	reader, fileSize, err := getReaderForPath(filesystem, innerPath)
	if err != nil {
		return fmt.Errorf("accessing %s: %w", innerPath, err)
	}

	// Wrap with decryption if needed
	if crypto != nil {
		reader, err = wrapWithDecryption(reader, fileSize, crypto)
		if err != nil {
			return fmt.Errorf("setting up decryption for %s: %w", innerPath, err)
		}
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

	// Recursively execute the command (default = info)
	return runCommand(innerFS, remainingArgs, stdout, stderr)
}

// runFreeCat copies free space to stdout
func runFreeCat(filesystem fsys.FS, out io.Writer) error {
	fb, ok := filesystem.(fsys.FreeBlocker)
	if !ok {
		return fmt.Errorf("filesystem type %s does not support free block listing", filesystem.Type())
	}

	ranges, err := fb.FreeBlocks()
	if err != nil {
		return fmt.Errorf("getting free blocks: %w", err)
	}

	br, ok := filesystem.(interface{ BaseReader() io.ReaderAt })
	if !ok {
		return fmt.Errorf("filesystem does not expose base reader")
	}

	// Convert ranges to extents
	extents := make([]fsys.Extent, len(ranges))
	var totalSize int64
	for i, r := range ranges {
		extents[i] = fsys.Extent{
			Logical:  totalSize,
			Physical: r.Start,
			Length:   r.Size(),
		}
		totalSize += r.Size()
	}

	reader := fsys.NewExtentReaderAt(br.BaseReader(), extents, totalSize)
	return streamToWriter(reader, totalSize, out)
}

// runFreeFscat probes free space as a filesystem image
func runFreeFscat(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	fb, ok := filesystem.(fsys.FreeBlocker)
	if !ok {
		return fmt.Errorf("filesystem type %s does not support free block listing", filesystem.Type())
	}

	ranges, err := fb.FreeBlocks()
	if err != nil {
		return fmt.Errorf("getting free blocks: %w", err)
	}

	br, ok := filesystem.(interface{ BaseReader() io.ReaderAt })
	if !ok {
		return fmt.Errorf("filesystem does not expose base reader")
	}

	// Convert ranges to extents
	extents := make([]fsys.Extent, len(ranges))
	var totalSize int64
	for i, r := range ranges {
		extents[i] = fsys.Extent{
			Logical:  totalSize,
			Physical: r.Start,
			Length:   r.Size(),
		}
		totalSize += r.Size()
	}

	reader := fsys.NewExtentReaderAt(br.BaseReader(), extents, totalSize)

	// Detect filesystem type
	fsType, err := detect.Detect(reader)
	if err != nil {
		return fmt.Errorf("detecting filesystem in free space: %w", err)
	}

	if fsType == detect.Unknown {
		return fmt.Errorf("no recognizable filesystem in free space")
	}

	// Open the filesystem
	innerFS, err := openFilesystem(reader, totalSize, fsType)
	if err != nil {
		return fmt.Errorf("opening filesystem in free space: %w", err)
	}
	defer innerFS.Close()

	return runCommand(innerFS, args, stdout, stderr)
}

// runNbd exposes a file as an NBD block device
func runNbd(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	flagSet := flag.NewFlagSet("nbd", flag.ContinueOnError)
	socketPath := flagSet.String("socket", "/tmp/nbd.sock", "Unix socket path")
	exportName := flagSet.String("name", "export", "Export name for NBD clients")
	readWrite := flagSet.Bool("rw", false, "Enable read-write access")
	keyHex := flagSet.String("K", "", "XTS-AES key in hexadecimal")
	sectorSize := flagSet.Int("sector", 512, "Sector size for XTS encryption")
	tweakOffset := flagSet.Uint64("tweak-offset", 0, "Starting tweak value offset")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	if flagSet.NArg() < 1 {
		return fmt.Errorf("nbd requires a path argument")
	}

	// Parse crypto params
	var crypto *cryptoParams
	if *keyHex != "" {
		key, err := hex.DecodeString(*keyHex)
		if err != nil {
			return fmt.Errorf("invalid key hex: %w", err)
		}
		crypto = &cryptoParams{
			key:         key,
			sectorSize:  *sectorSize,
			tweakOffset: *tweakOffset,
		}
	}

	path := flagSet.Arg(0)
	reader, size, err := getReaderForPath(filesystem, path)
	if err != nil {
		return err
	}

	// Wrap with decryption if needed
	if crypto != nil {
		reader, err = wrapWithDecryption(reader, size, crypto)
		if err != nil {
			return fmt.Errorf("setting up decryption: %w", err)
		}
	}

	var writer io.WriterAt
	if *readWrite {
		writer, err = getWriterForReader(reader)
		if err != nil {
			return fmt.Errorf("cannot enable write access: %w", err)
		}
	}

	return serveNbd(*socketPath, *exportName, reader, writer, size, stdout, stderr)
}

// runFreeNbd exposes free space as an NBD block device
func runFreeNbd(filesystem fsys.FS, args []string, stdout, stderr io.Writer) error {
	flagSet := flag.NewFlagSet("freenbd", flag.ContinueOnError)
	socketPath := flagSet.String("socket", "/tmp/nbd.sock", "Unix socket path")
	exportName := flagSet.String("name", "freespace", "Export name for NBD clients")
	readWrite := flagSet.Bool("rw", false, "Enable read-write access")
	if err := flagSet.Parse(args); err != nil {
		return err
	}

	fb, ok := filesystem.(fsys.FreeBlocker)
	if !ok {
		return fmt.Errorf("filesystem type %s does not support free block listing", filesystem.Type())
	}

	ranges, err := fb.FreeBlocks()
	if err != nil {
		return fmt.Errorf("getting free blocks: %w", err)
	}

	br, ok := filesystem.(interface{ BaseReader() io.ReaderAt })
	if !ok {
		return fmt.Errorf("filesystem does not expose base reader")
	}

	// Convert ranges to extents
	extents := make([]fsys.Extent, len(ranges))
	var totalSize int64
	for i, r := range ranges {
		extents[i] = fsys.Extent{
			Logical:  totalSize,
			Physical: r.Start,
			Length:   r.Size(),
		}
		totalSize += r.Size()
	}

	reader := fsys.NewExtentReaderAt(br.BaseReader(), extents, totalSize)

	var writer io.WriterAt
	if *readWrite {
		writer, err = getWriterForReader(reader)
		if err != nil {
			return fmt.Errorf("cannot enable write access: %w", err)
		}
	}

	return serveNbd(*socketPath, *exportName, reader, writer, totalSize, stdout, stderr)
}

// getWriterForReader creates a writer that uses the same extent map as the reader.
// It requires the underlying base reader to be an *os.File so it can be re-opened for writing.
// getWriterForReader creates a writer that uses the same extent map and encryption as the reader.
// It unwraps XTS and extent layers to find the base file, then rebuilds the write chain.
func getWriterForReader(reader io.ReaderAt) (io.WriterAt, error) {
	// Unwrap layers to find base file and collect XTS cipher if present
	var xtsCipher *xts.Cipher
	var xtsSize int64
	current := reader

	// Check for XTS layer first
	if xtsReader, ok := current.(*xts.ReaderAt); ok {
		xtsCipher = xtsReader.Cipher()
		xtsSize = xtsReader.Size()
		current = xtsReader.BaseReader()
	}

	// Check for extent layer
	var extents []fsys.Extent
	var extentSize int64
	if extReader, ok := current.(*fsys.ExtentReaderAt); ok {
		extents = extReader.Extents()
		extentSize = extReader.Size()
		current = extReader.BaseReader()
	}

	// Now we should have the base file
	baseFile, ok := current.(*os.File)
	if !ok {
		return nil, fmt.Errorf("base reader is not a file (nested read-write not supported through memory buffers)")
	}

	// Re-open the file in read-write mode
	rwFile, err := os.OpenFile(baseFile.Name(), os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("opening file for writing: %w", err)
	}

	// Rebuild the write chain
	var writer io.WriterAt = rwFile

	// Add extent layer if present
	if len(extents) > 0 {
		writer = fsys.NewExtentWriterAt(writer, extents, extentSize)
	}

	// Add XTS layer if present
	if xtsCipher != nil {
		size := xtsSize
		if size == 0 {
			size = extentSize
		}
		writer = xts.NewWriterAt(writer, xtsCipher, size)
	}

	return writer, nil
}

// serveNbd starts an NBD server with the given reader and optional writer
func serveNbd(socketPath, exportName string, reader io.ReaderAt, writer io.WriterAt, size int64, stdout, stderr io.Writer) error {
	server := nbd.NewServer(socketPath)

	exp := &nbd.Export{
		Name:   exportName,
		Reader: reader,
		Writer: writer,
		Size:   size,
	}

	if err := server.AddExport(exp); err != nil {
		return err
	}

	// Handle shutdown signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(stderr, "\nShutting down...")
		server.Close()
	}()

	rwStr := "read-only"
	if writer != nil {
		rwStr = "read-write"
	}

	fmt.Fprintf(stdout, "NBD server starting on unix:%s\n", socketPath)
	fmt.Fprintf(stdout, "Export: %s (%d bytes, %s)\n", exportName, size, rwStr)
	fmt.Fprintf(stdout, "Connect with: sudo nbd-client -N %s -unix %s /dev/nbdX\n", exportName, socketPath)
	fmt.Fprintf(stdout, "Press Ctrl+C to stop\n")

	return server.Serve()
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
	case fsType == detect.APFS:
		return apfs.Open(r, size)
	case fsType == detect.HFSPlus:
		return hfsplus.Open(r, size)
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

	// Check if path is a file or directory
	info, err := filesystem.Stat(path)
	if err != nil {
		return err
	}

	if !info.IsDir() {
		// It's a file - just show its info
		if *long {
			fmt.Fprintf(out, "%s %12d %s %s\n",
				info.Mode(), info.Size(), info.ModTime().Format("Jan _2 15:04"), info.Name())
		} else {
			fmt.Fprintln(out, info.Name())
		}
		return nil
	}

	// It's a directory - list contents
	entries, err := filesystem.ReadDir(path)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		// Skip system files unless -a
		if !*all && isSystemFile(entry.Name()) {
			continue
		}

		if *long {
			einfo, err := entry.Info()
			if err != nil {
				continue
			}
			fmt.Fprintf(out, "%s %12d %s %s\n",
				einfo.Mode(), einfo.Size(), einfo.ModTime().Format("Jan _2 15:04"), entry.Name())
		} else {
			name := entry.Name()
			if entry.IsDir() {
				name += "/"
			}
			fmt.Fprintln(out, name)
		}
	}

	return nil
}

func isSystemFile(name string) bool {
	// NTFS system files
	if len(name) > 0 && name[0] == '$' {
		return true
	}
	return false
}

func runCat(filesystem fsys.FS, args []string, out io.Writer) error {
	if len(args) < 1 {
		return fmt.Errorf("cat requires a path argument")
	}

	path := args[0]
	reader, size, err := getReaderForPath(filesystem, path)
	if err != nil {
		return err
	}

	return streamToWriter(reader, size, out)
}

// streamToWriter copies from ReaderAt to Writer
func streamToWriter(r io.ReaderAt, size int64, out io.Writer) error {
	const bufSize = 64 * 1024
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

func runInfo(filesystem fsys.FS, out io.Writer) error {
	fmt.Fprintf(out, "Filesystem: %s\n", filesystem.Type())

	// Check if filesystem has detailed info
	type infoProvider interface {
		Info() string
	}
	if ip, ok := filesystem.(infoProvider); ok {
		fmt.Fprintln(out)
		fmt.Fprintln(out, ip.Info())
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
