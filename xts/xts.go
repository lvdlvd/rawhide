// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package xts implements the XTS cipher mode as specified in IEEE P1619/D16.
//
// XTS mode is typically used for disk encryption. The disk is conceptually
// an array of sectors and we must be able to encrypt and decrypt a sector
// in isolation. XTS wraps a block cipher with Rogaway's XEX mode in order
// to build a tweakable block cipher, allowing each sector to have a unique
// tweak and effectively creating a unique key for each sector.
//
// This implementation is adapted from golang.org/x/crypto/xts with added
// support for configurable sector sizes and ReaderAt/WriterAt wrappers.
package xts

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// blockSize is the block size that the underlying cipher must have.
const blockSize = 16

// Cipher contains an expanded key structure.
type Cipher struct {
	k1, k2     cipher.Block
	sectorSize int
}

// NewCipher creates a Cipher given a function for creating the underlying
// block cipher (which must have a block size of 16 bytes). The key must be
// twice the length of the underlying cipher's key.
func NewCipher(cipherFunc func([]byte) (cipher.Block, error), key []byte) (*Cipher, error) {
	c := new(Cipher)
	var err error
	if c.k1, err = cipherFunc(key[:len(key)/2]); err != nil {
		return nil, err
	}
	if c.k2, err = cipherFunc(key[len(key)/2:]); err != nil {
		return nil, err
	}

	if c.k1.BlockSize() != blockSize {
		return nil, errors.New("xts: cipher does not have a block size of 16")
	}

	c.sectorSize = blockSize // Default for compatibility
	return c, nil
}

// New creates an XTS-AES cipher with the given key and sector size.
// Key must be 32 bytes (AES-128-XTS), 48 bytes (AES-192-XTS), or 64 bytes (AES-256-XTS).
// The key is split in half: first half for data encryption, second half for tweak.
// Sector size must be a positive multiple of 16 bytes.
func New(key []byte, sectorSize int) (*Cipher, error) {
	if len(key) != 32 && len(key) != 48 && len(key) != 64 {
		return nil, fmt.Errorf("xts: invalid key length %d (must be 32, 48, or 64)", len(key))
	}
	if sectorSize < blockSize || sectorSize%blockSize != 0 {
		return nil, fmt.Errorf("xts: sector size must be a positive multiple of %d", blockSize)
	}

	c, err := NewCipher(aes.NewCipher, key)
	if err != nil {
		return nil, err
	}
	c.sectorSize = sectorSize
	return c, nil
}

// SectorSize returns the sector size.
func (c *Cipher) SectorSize() int {
	return c.sectorSize
}

// Encrypt encrypts a sector of plaintext and puts the result into ciphertext.
// Plaintext and ciphertext must overlap entirely or not at all.
// Sectors must be a multiple of 16 bytes.
func (c *Cipher) Encrypt(ciphertext, plaintext []byte, sectorNum uint64) {
	if len(ciphertext) < len(plaintext) {
		panic("xts: ciphertext is smaller than plaintext")
	}
	if len(plaintext)%blockSize != 0 {
		panic("xts: plaintext is not a multiple of the block size")
	}

	var tweak [blockSize]byte
	binary.LittleEndian.PutUint64(tweak[:8], sectorNum)
	c.k2.Encrypt(tweak[:], tweak[:])

	for i := 0; i < len(plaintext); i += blockSize {
		for j := 0; j < blockSize; j++ {
			ciphertext[i+j] = plaintext[i+j] ^ tweak[j]
		}
		c.k1.Encrypt(ciphertext[i:i+blockSize], ciphertext[i:i+blockSize])
		for j := 0; j < blockSize; j++ {
			ciphertext[i+j] ^= tweak[j]
		}
		mul2(&tweak)
	}
}

// Decrypt decrypts a sector of ciphertext and puts the result into plaintext.
// Plaintext and ciphertext must overlap entirely or not at all.
// Sectors must be a multiple of 16 bytes.
func (c *Cipher) Decrypt(plaintext, ciphertext []byte, sectorNum uint64) {
	if len(plaintext) < len(ciphertext) {
		panic("xts: plaintext is smaller than ciphertext")
	}
	if len(ciphertext)%blockSize != 0 {
		panic("xts: ciphertext is not a multiple of the block size")
	}

	var tweak [blockSize]byte
	binary.LittleEndian.PutUint64(tweak[:8], sectorNum)
	c.k2.Encrypt(tweak[:], tweak[:])

	for i := 0; i < len(ciphertext); i += blockSize {
		for j := 0; j < blockSize; j++ {
			plaintext[i+j] = ciphertext[i+j] ^ tweak[j]
		}
		c.k1.Decrypt(plaintext[i:i+blockSize], plaintext[i:i+blockSize])
		for j := 0; j < blockSize; j++ {
			plaintext[i+j] ^= tweak[j]
		}
		mul2(&tweak)
	}
}

// mul2 multiplies tweak by 2 in GF(2^128) with an irreducible polynomial of
// x^128 + x^7 + x^2 + x + 1.
func mul2(tweak *[blockSize]byte) {
	var carryIn byte
	for j := range tweak {
		carryOut := tweak[j] >> 7
		tweak[j] = (tweak[j] << 1) + carryIn
		carryIn = carryOut
	}
	if carryIn != 0 {
		// If we have a carry bit then we need to subtract a multiple
		// of the irreducible polynomial (x^128 + x^7 + x^2 + x + 1).
		// By dropping the carry bit, we're subtracting the x^128 term
		// so all that remains is to subtract x^7 + x^2 + x + 1.
		// Subtraction (and addition) in this representation is just XOR.
		tweak[0] ^= 1<<7 | 1<<2 | 1<<1 | 1
	}
}

// EncryptSector encrypts a single sector in place.
func (c *Cipher) EncryptSector(sector []byte, sectorNum uint64) error {
	if len(sector) != c.sectorSize {
		return fmt.Errorf("xts: sector length %d != sector size %d", len(sector), c.sectorSize)
	}
	c.Encrypt(sector, sector, sectorNum)
	return nil
}

// DecryptSector decrypts a single sector in place.
func (c *Cipher) DecryptSector(sector []byte, sectorNum uint64) error {
	if len(sector) != c.sectorSize {
		return fmt.Errorf("xts: sector length %d != sector size %d", len(sector), c.sectorSize)
	}
	c.Decrypt(sector, sector, sectorNum)
	return nil
}

// EncryptSectors encrypts multiple sectors in place.
func (c *Cipher) EncryptSectors(data []byte, startSector uint64) error {
	if len(data)%c.sectorSize != 0 {
		return fmt.Errorf("xts: data length %d not a multiple of sector size %d", len(data), c.sectorSize)
	}
	for i := 0; i < len(data); i += c.sectorSize {
		c.Encrypt(data[i:i+c.sectorSize], data[i:i+c.sectorSize], startSector)
		startSector++
	}
	return nil
}

// DecryptSectors decrypts multiple sectors in place.
func (c *Cipher) DecryptSectors(data []byte, startSector uint64) error {
	if len(data)%c.sectorSize != 0 {
		return fmt.Errorf("xts: data length %d not a multiple of sector size %d", len(data), c.sectorSize)
	}
	for i := 0; i < len(data); i += c.sectorSize {
		c.Decrypt(data[i:i+c.sectorSize], data[i:i+c.sectorSize], startSector)
		startSector++
	}
	return nil
}

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
			return 0, fmt.Errorf("xts: partial sector read (%d bytes)", readN)
		}
		return 0, io.EOF
	}

	decryptLen := completeSectors * int(sectorSize)
	if err := x.cipher.DecryptSectors(alignedBuf[:decryptLen], uint64(startSector)); err != nil {
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
// Writes must be sector-aligned and sector-sized.
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
	if err := x.cipher.EncryptSectors(encrypted, startSector); err != nil {
		return 0, fmt.Errorf("xts: encryption failed: %w", err)
	}

	// Write encrypted data
	return x.w.WriteAt(encrypted, off)
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
