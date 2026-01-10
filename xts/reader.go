package xts

import (
	"fmt"
	"io"
)

// ReaderAt wraps an io.ReaderAt and decrypts data on read using XTS-AES.
type ReaderAt struct {
	r      io.ReaderAt
	cipher *Cipher
	size   int64
}

// NewReaderAt creates a new decrypting ReaderAt.
func NewReaderAt(r io.ReaderAt, cipher *Cipher, size int64) *ReaderAt {
	return &ReaderAt{
		r:      r,
		cipher: cipher,
		size:   size,
	}
}

// ReadAt implements io.ReaderAt with decryption.
func (x *ReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, fmt.Errorf("xts: negative offset")
	}
	if off >= x.size {
		return 0, io.EOF
	}

	sectorSize := int64(x.cipher.SectorSize())

	// Calculate sector-aligned read boundaries
	startSector := off / sectorSize
	endOffset := off + int64(len(p))
	if endOffset > x.size {
		endOffset = x.size
	}
	endSector := (endOffset + sectorSize - 1) / sectorSize

	// Read sector-aligned data
	alignedStart := startSector * sectorSize
	alignedLen := (endSector - startSector) * sectorSize
	alignedBuf := make([]byte, alignedLen)

	readN, err := x.r.ReadAt(alignedBuf, alignedStart)
	if err != nil && err != io.EOF {
		return 0, err
	}

	// Round down to complete sectors for decryption
	completeSectors := readN / int(sectorSize)
	if completeSectors == 0 {
		if readN > 0 {
			// Partial sector read - can't decrypt properly
			return 0, fmt.Errorf("xts: partial sector read (%d bytes)", readN)
		}
		return 0, io.EOF
	}

	decryptLen := completeSectors * int(sectorSize)
	if err := x.cipher.Decrypt(alignedBuf[:decryptLen], uint64(startSector)); err != nil {
		return 0, fmt.Errorf("xts: decryption failed: %w", err)
	}

	// Copy the requested portion to output
	offsetInBuf := int(off - alignedStart)
	available := decryptLen - offsetInBuf
	toCopy := len(p)
	if toCopy > available {
		toCopy = available
	}
	copy(p[:toCopy], alignedBuf[offsetInBuf:offsetInBuf+toCopy])

	if off+int64(toCopy) >= x.size {
		return toCopy, io.EOF
	}
	return toCopy, nil
}

// BaseReader returns the underlying reader.
func (x *ReaderAt) BaseReader() io.ReaderAt {
	return x.r
}

// Cipher returns the XTS cipher (for creating a matching writer).
func (x *ReaderAt) Cipher() *Cipher {
	return x.cipher
}

// Size returns the logical size.
func (x *ReaderAt) Size() int64 {
	return x.size
}
