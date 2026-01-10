// Package xts implements XTS-AES (XEX-based tweaked-codebook mode with ciphertext stealing)
// as specified in IEEE Std 1619-2007.
package xts

import (
	"crypto/aes"
	"crypto/cipher"
	"encoding/binary"
	"errors"
	"fmt"
)

// Cipher implements XTS-AES encryption/decryption
type Cipher struct {
	k1, k2      cipher.Block // K1 for data encryption, K2 for tweak encryption
	sectorSize  int          // Size of each data unit (sector)
	tweakOffset uint64       // Starting tweak value (logical sector number offset)
}

// New creates a new XTS-AES cipher.
// Key must be 32 bytes (AES-128-XTS), 48 bytes (AES-192-XTS), or 64 bytes (AES-256-XTS).
// The key is split in half: first half for data encryption, second half for tweak.
func New(key []byte, sectorSize int, tweakOffset uint64) (*Cipher, error) {
	keyLen := len(key)
	if keyLen != 32 && keyLen != 48 && keyLen != 64 {
		return nil, fmt.Errorf("xts: invalid key length %d (must be 32, 48, or 64 bytes)", keyLen)
	}

	if sectorSize < 16 {
		return nil, errors.New("xts: sector size must be at least 16 bytes")
	}

	halfLen := keyLen / 2
	k1, err := aes.NewCipher(key[:halfLen])
	if err != nil {
		return nil, fmt.Errorf("xts: creating K1 cipher: %w", err)
	}

	k2, err := aes.NewCipher(key[halfLen:])
	if err != nil {
		return nil, fmt.Errorf("xts: creating K2 cipher: %w", err)
	}

	return &Cipher{
		k1:          k1,
		k2:          k2,
		sectorSize:  sectorSize,
		tweakOffset: tweakOffset,
	}, nil
}

// SectorSize returns the sector size
func (c *Cipher) SectorSize() int {
	return c.sectorSize
}

// TweakOffset returns the tweak offset
func (c *Cipher) TweakOffset() uint64 {
	return c.tweakOffset
}

// EncryptSector encrypts a single sector in place.
// sectorNum is the logical sector number (tweak value before adding offset).
func (c *Cipher) EncryptSector(data []byte, sectorNum uint64) error {
	if len(data) != c.sectorSize {
		return fmt.Errorf("xts: data length %d != sector size %d", len(data), c.sectorSize)
	}
	return c.process(data, sectorNum+c.tweakOffset, false)
}

// DecryptSector decrypts a single sector in place.
// sectorNum is the logical sector number (tweak value before adding offset).
func (c *Cipher) DecryptSector(data []byte, sectorNum uint64) error {
	if len(data) != c.sectorSize {
		return fmt.Errorf("xts: data length %d != sector size %d", len(data), c.sectorSize)
	}
	return c.process(data, sectorNum+c.tweakOffset, true)
}

// process performs XTS encryption or decryption
func (c *Cipher) process(data []byte, tweak uint64, decrypt bool) error {
	// Encrypt the tweak
	var tweakBuf [16]byte
	binary.LittleEndian.PutUint64(tweakBuf[:8], tweak)
	// Upper 64 bits are zero (standard XTS uses 128-bit tweak, lower bits are sector number)

	var T [16]byte
	c.k2.Encrypt(T[:], tweakBuf[:])

	// Process each 16-byte block
	numBlocks := len(data) / 16
	for i := 0; i < numBlocks; i++ {
		block := data[i*16 : (i+1)*16]

		// XOR with T
		xorBlock(block, T[:])

		// Encrypt or decrypt
		if decrypt {
			c.k1.Decrypt(block, block)
		} else {
			c.k1.Encrypt(block, block)
		}

		// XOR with T again
		xorBlock(block, T[:])

		// Multiply T by x in GF(2^128)
		gfMul(T[:])
	}

	return nil
}

// xorBlock XORs b with x in place
func xorBlock(b, x []byte) {
	for i := 0; i < 16; i++ {
		b[i] ^= x[i]
	}
}

// gfMul multiplies the value in b by x in GF(2^128) with polynomial x^128 + x^7 + x^2 + x + 1
func gfMul(b []byte) {
	var carry byte
	for i := 0; i < 16; i++ {
		newCarry := b[i] >> 7
		b[i] = (b[i] << 1) | carry
		carry = newCarry
	}
	// If there was a carry out, XOR with the reduction polynomial (0x87)
	if carry != 0 {
		b[0] ^= 0x87
	}
}

// Encrypt encrypts multiple sectors.
// data length must be a multiple of sector size.
// startSector is the logical sector number of the first sector.
func (c *Cipher) Encrypt(data []byte, startSector uint64) error {
	if len(data)%c.sectorSize != 0 {
		return fmt.Errorf("xts: data length %d not a multiple of sector size %d", len(data), c.sectorSize)
	}

	numSectors := len(data) / c.sectorSize
	for i := 0; i < numSectors; i++ {
		sector := data[i*c.sectorSize : (i+1)*c.sectorSize]
		if err := c.EncryptSector(sector, startSector+uint64(i)); err != nil {
			return err
		}
	}
	return nil
}

// Decrypt decrypts multiple sectors.
// data length must be a multiple of sector size.
// startSector is the logical sector number of the first sector.
func (c *Cipher) Decrypt(data []byte, startSector uint64) error {
	if len(data)%c.sectorSize != 0 {
		return fmt.Errorf("xts: data length %d not a multiple of sector size %d", len(data), c.sectorSize)
	}

	numSectors := len(data) / c.sectorSize
	for i := 0; i < numSectors; i++ {
		sector := data[i*c.sectorSize : (i+1)*c.sectorSize]
		if err := c.DecryptSector(sector, startSector+uint64(i)); err != nil {
			return err
		}
	}
	return nil
}
