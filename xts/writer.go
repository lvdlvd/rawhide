package xts

import (
	"fmt"
	"io"
)

// WriterAt wraps an io.WriterAt and encrypts data on write using XTS-AES.
type WriterAt struct {
	w      io.WriterAt
	cipher *Cipher
	size   int64
}

// NewWriterAt creates a new encrypting WriterAt.
func NewWriterAt(w io.WriterAt, cipher *Cipher, size int64) *WriterAt {
	return &WriterAt{
		w:      w,
		cipher: cipher,
		size:   size,
	}
}

// WriteAt implements io.WriterAt with encryption.
// Note: For simplicity, writes must be sector-aligned and sector-sized.
// Partial sector writes would require read-modify-write which needs a ReaderAt too.
func (x *WriterAt) WriteAt(p []byte, off int64) (n int, err error) {
	if off < 0 {
		return 0, fmt.Errorf("xts: negative offset")
	}
	if off >= x.size {
		return 0, io.ErrShortWrite
	}

	sectorSize := int64(x.cipher.SectorSize())

	// Check alignment
	if off%sectorSize != 0 {
		return 0, fmt.Errorf("xts: write offset %d not sector-aligned (sector size %d)", off, sectorSize)
	}
	if int64(len(p))%sectorSize != 0 {
		return 0, fmt.Errorf("xts: write length %d not a multiple of sector size %d", len(p), sectorSize)
	}

	// Limit to size
	writeLen := int64(len(p))
	if off+writeLen > x.size {
		writeLen = x.size - off
	}

	// Make a copy for encryption (don't modify caller's buffer)
	encrypted := make([]byte, writeLen)
	copy(encrypted, p[:writeLen])

	// Encrypt
	startSector := uint64(off / sectorSize)
	if err := x.cipher.Encrypt(encrypted, startSector); err != nil {
		return 0, fmt.Errorf("xts: encryption failed: %w", err)
	}

	// Write encrypted data
	written, err := x.w.WriteAt(encrypted, off)
	return written, err
}

// BaseWriter returns the underlying writer.
func (x *WriterAt) BaseWriter() io.WriterAt {
	return x.w
}

// Cipher returns the XTS cipher.
func (x *WriterAt) Cipher() *Cipher {
	return x.cipher
}

// Size returns the logical size.
func (x *WriterAt) Size() int64 {
	return x.size
}
