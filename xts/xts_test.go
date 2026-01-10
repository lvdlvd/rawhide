package xts

import (
	"bytes"
	"encoding/hex"
	"io"
	"testing"
)

// Test vectors from IEEE Std 1619-2007
func TestXTSVectors(t *testing.T) {
	tests := []struct {
		name       string
		key        string // hex
		tweak      uint64
		plaintext  string // hex
		ciphertext string // hex
		sectorSize int
	}{
		{
			// IEEE 1619-2007 Vector 1 (AES-128-XTS)
			name:       "IEEE Vector 1",
			key:        "0000000000000000000000000000000000000000000000000000000000000000",
			tweak:      0,
			plaintext:  "0000000000000000000000000000000000000000000000000000000000000000",
			ciphertext: "917cf69ebd68b2ec9b9fe9a3eadda692cd43d2f59598ed858c02c2652fbf922e",
			sectorSize: 32,
		},
		{
			// IEEE 1619-2007 Vector 2 (AES-128-XTS)
			name:       "IEEE Vector 2",
			key:        "1111111111111111111111111111111122222222222222222222222222222222",
			tweak:      0x3333333333,
			plaintext:  "4444444444444444444444444444444444444444444444444444444444444444",
			ciphertext: "c454185e6a16936e39334038acef838bfb186fff7480adc4289382ecd6d394f0",
			sectorSize: 32,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, _ := hex.DecodeString(tt.key)
			plaintext, _ := hex.DecodeString(tt.plaintext)
			expected, _ := hex.DecodeString(tt.ciphertext)

			cipher, err := New(key, tt.sectorSize, 0)
			if err != nil {
				t.Fatalf("New() error: %v", err)
			}

			// Test encryption
			encrypted := make([]byte, len(plaintext))
			copy(encrypted, plaintext)

			// Use EncryptSector for single-sector tests
			if len(plaintext) == tt.sectorSize {
				err = cipher.EncryptSector(encrypted, tt.tweak)
			} else {
				err = cipher.Encrypt(encrypted, tt.tweak)
			}
			if err != nil {
				t.Fatalf("Encrypt error: %v", err)
			}

			if !bytes.Equal(encrypted, expected) {
				t.Errorf("Encrypt mismatch:\ngot:  %x\nwant: %x", encrypted, expected)
			}

			// Test decryption
			decrypted := make([]byte, len(expected))
			copy(decrypted, expected)

			if len(expected) == tt.sectorSize {
				err = cipher.DecryptSector(decrypted, tt.tweak)
			} else {
				err = cipher.Decrypt(decrypted, tt.tweak)
			}
			if err != nil {
				t.Fatalf("Decrypt error: %v", err)
			}

			if !bytes.Equal(decrypted, plaintext) {
				t.Errorf("Decrypt mismatch:\ngot:  %x\nwant: %x", decrypted, plaintext)
			}
		})
	}
}

func TestXTSKeyLengths(t *testing.T) {
	tests := []struct {
		keyLen  int
		wantErr bool
	}{
		{16, true},  // Too short
		{32, false}, // AES-128-XTS
		{48, false}, // AES-192-XTS
		{64, false}, // AES-256-XTS
		{128, true}, // Too long
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			key := make([]byte, tt.keyLen)
			_, err := New(key, 512, 0)
			if (err != nil) != tt.wantErr {
				t.Errorf("keyLen=%d: got err=%v, wantErr=%v", tt.keyLen, err, tt.wantErr)
			}
		})
	}
}

func TestXTSTweakOffset(t *testing.T) {
	key := make([]byte, 32)
	plaintext := make([]byte, 512)
	for i := range plaintext {
		plaintext[i] = byte(i)
	}

	// Encrypt with tweak offset 0, sector 100
	c1, _ := New(key, 512, 0)
	enc1 := make([]byte, 512)
	copy(enc1, plaintext)
	c1.EncryptSector(enc1, 100)

	// Encrypt with tweak offset 100, sector 0 (should be same result)
	c2, _ := New(key, 512, 100)
	enc2 := make([]byte, 512)
	copy(enc2, plaintext)
	c2.EncryptSector(enc2, 0)

	if !bytes.Equal(enc1, enc2) {
		t.Error("Tweak offset not applied correctly")
	}
}

// bytesBuffer for testing
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(b.data)) {
		return 0, io.EOF
	}
	n := copy(p, b.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func (b *bytesBuffer) WriteAt(p []byte, off int64) (int, error) {
	if int(off)+len(p) > len(b.data) {
		return 0, io.ErrShortWrite
	}
	copy(b.data[off:], p)
	return len(p), nil
}

func TestReaderAt(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	// Create plaintext data (2 sectors)
	plaintext := make([]byte, 1024)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	// Encrypt it
	cipher, _ := New(key, 512, 0)
	encrypted := make([]byte, 1024)
	copy(encrypted, plaintext)
	cipher.Encrypt(encrypted, 0)

	// Create reader
	buf := &bytesBuffer{data: encrypted}
	reader := NewReaderAt(buf, cipher, 1024)

	// Read and verify
	decrypted := make([]byte, 1024)
	n, err := reader.ReadAt(decrypted, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt error: %v", err)
	}
	if n != 1024 {
		t.Fatalf("ReadAt returned %d bytes, want 1024", n)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Error("Decrypted data doesn't match plaintext")
	}
}

func TestReaderAtUnaligned(t *testing.T) {
	key := make([]byte, 32)

	// Create plaintext data (2 sectors)
	plaintext := make([]byte, 1024)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	// Encrypt it
	cipher, _ := New(key, 512, 0)
	encrypted := make([]byte, 1024)
	copy(encrypted, plaintext)
	cipher.Encrypt(encrypted, 0)

	// Create reader
	buf := &bytesBuffer{data: encrypted}
	reader := NewReaderAt(buf, cipher, 1024)

	// Read from middle of first sector
	partial := make([]byte, 100)
	n, err := reader.ReadAt(partial, 100)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if n != 100 {
		t.Fatalf("ReadAt returned %d bytes, want 100", n)
	}
	if !bytes.Equal(partial, plaintext[100:200]) {
		t.Error("Unaligned read doesn't match")
	}

	// Read across sector boundary
	cross := make([]byte, 200)
	n, err = reader.ReadAt(cross, 450)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if n != 200 {
		t.Fatalf("ReadAt returned %d bytes, want 200", n)
	}
	if !bytes.Equal(cross, plaintext[450:650]) {
		t.Error("Cross-sector read doesn't match")
	}
}

func TestWriterAt(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	// Create plaintext
	plaintext := make([]byte, 512)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	// Write through encrypting writer
	cipher, _ := New(key, 512, 0)
	buf := &bytesBuffer{data: make([]byte, 512)}
	writer := NewWriterAt(buf, cipher, 512)

	n, err := writer.WriteAt(plaintext, 0)
	if err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	if n != 512 {
		t.Fatalf("WriteAt returned %d, want 512", n)
	}

	// Read back and decrypt to verify
	reader := NewReaderAt(buf, cipher, 512)
	decrypted := make([]byte, 512)
	reader.ReadAt(decrypted, 0)

	if !bytes.Equal(decrypted, plaintext) {
		t.Error("Write/Read roundtrip failed")
	}
}

func TestReaderWriterRoundtrip(t *testing.T) {
	key, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")

	// Original plaintext
	plaintext := make([]byte, 2048)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	cipher, _ := New(key, 512, 0)
	buf := &bytesBuffer{data: make([]byte, 2048)}

	// Write all sectors
	writer := NewWriterAt(buf, cipher, 2048)
	for i := 0; i < 4; i++ {
		_, err := writer.WriteAt(plaintext[i*512:(i+1)*512], int64(i*512))
		if err != nil {
			t.Fatalf("WriteAt sector %d error: %v", i, err)
		}
	}

	// Read back and verify
	reader := NewReaderAt(buf, cipher, 2048)
	decrypted := make([]byte, 2048)
	n, err := reader.ReadAt(decrypted, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt error: %v", err)
	}
	if n != 2048 {
		t.Fatalf("ReadAt returned %d, want 2048", n)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Error("Roundtrip failed")
	}
}

func TestXTS512ByteSectorRoundtrip(t *testing.T) {
	// Test with 512-byte sectors (32 AES blocks per sector)
	key, _ := hex.DecodeString("000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f")

	plaintext := make([]byte, 512)
	for i := range plaintext {
		plaintext[i] = byte(i % 256)
	}

	cipher, _ := New(key, 512, 0)

	// Encrypt
	encrypted := make([]byte, 512)
	copy(encrypted, plaintext)
	if err := cipher.EncryptSector(encrypted, 0); err != nil {
		t.Fatalf("EncryptSector error: %v", err)
	}

	// Verify ciphertext is different from plaintext
	if bytes.Equal(encrypted, plaintext) {
		t.Error("Ciphertext should differ from plaintext")
	}

	// Decrypt
	decrypted := make([]byte, 512)
	copy(decrypted, encrypted)
	if err := cipher.DecryptSector(decrypted, 0); err != nil {
		t.Fatalf("DecryptSector error: %v", err)
	}

	if !bytes.Equal(decrypted, plaintext) {
		t.Error("Decrypted data doesn't match plaintext")
	}
}

func TestXTSMultipleSectors(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}

	// Test that different sectors produce different ciphertext for same plaintext
	plaintext := make([]byte, 512)
	for i := range plaintext {
		plaintext[i] = 0xAA
	}

	cipher, _ := New(key, 512, 0)

	enc0 := make([]byte, 512)
	copy(enc0, plaintext)
	cipher.EncryptSector(enc0, 0)

	enc1 := make([]byte, 512)
	copy(enc1, plaintext)
	cipher.EncryptSector(enc1, 1)

	if bytes.Equal(enc0, enc1) {
		t.Error("Different sectors should produce different ciphertext")
	}

	// Verify each decrypts correctly
	cipher.DecryptSector(enc0, 0)
	cipher.DecryptSector(enc1, 1)

	if !bytes.Equal(enc0, plaintext) || !bytes.Equal(enc1, plaintext) {
		t.Error("Decryption failed for multi-sector test")
	}
}
