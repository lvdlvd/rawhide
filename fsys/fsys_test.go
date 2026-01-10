package fsys

import (
	"bytes"
	"io"
	"reflect"
	"testing"
)

func TestComposeExtents(t *testing.T) {
	tests := []struct {
		name     string
		outer    []Extent
		inner    []Extent
		expected []Extent
	}{
		{
			name: "simple single extent",
			// outer: logical [0,100) -> inner logical [1000,1100)
			// inner: logical [1000,1100) -> physical [5000,5100)
			// composed: logical [0,100) -> physical [5000,5100)
			outer:    []Extent{{Logical: 0, Physical: 1000, Length: 100}},
			inner:    []Extent{{Logical: 1000, Physical: 5000, Length: 100}},
			expected: []Extent{{Logical: 0, Physical: 5000, Length: 100}},
		},
		{
			name: "outer subset of inner",
			// outer: logical [0,50) -> inner logical [1025,1075)
			// inner: logical [1000,1100) -> physical [5000,5100)
			// composed: logical [0,50) -> physical [5025,5075)
			outer:    []Extent{{Logical: 0, Physical: 1025, Length: 50}},
			inner:    []Extent{{Logical: 1000, Physical: 5000, Length: 100}},
			expected: []Extent{{Logical: 0, Physical: 5025, Length: 50}},
		},
		{
			name: "outer spans two inner extents",
			// outer: logical [0,100) -> inner logical [50,150)
			// inner: logical [0,100) -> physical [1000,1100), logical [100,200) -> physical [2000,2100)
			// composed: logical [0,50) -> physical [1050,1100), logical [50,100) -> physical [2000,2050)
			outer: []Extent{{Logical: 0, Physical: 50, Length: 100}},
			inner: []Extent{
				{Logical: 0, Physical: 1000, Length: 100},
				{Logical: 100, Physical: 2000, Length: 100},
			},
			expected: []Extent{
				{Logical: 0, Physical: 1050, Length: 50},
				{Logical: 50, Physical: 2000, Length: 50},
			},
		},
		{
			name: "multiple outer extents",
			// outer: logical [0,50) -> inner [0,50), logical [50,100) -> inner [100,150)
			// inner: logical [0,100) -> physical [1000,1100), logical [100,200) -> physical [2000,2100)
			// composed: logical [0,50) -> physical [1000,1050), logical [50,100) -> physical [2000,2050)
			outer: []Extent{
				{Logical: 0, Physical: 0, Length: 50},
				{Logical: 50, Physical: 100, Length: 50},
			},
			inner: []Extent{
				{Logical: 0, Physical: 1000, Length: 100},
				{Logical: 100, Physical: 2000, Length: 100},
			},
			expected: []Extent{
				{Logical: 0, Physical: 1000, Length: 50},
				{Logical: 50, Physical: 2000, Length: 50},
			},
		},
		{
			name: "three level simulation",
			// Simulating: partition at offset 1MB, file at cluster 10 (40KB), reading 4KB
			// outer (file in inner fs): [0, 4096) -> [40960, 45056)
			// inner (inner fs in partition): [0, 1048576) -> [1048576, 2097152)
			// The 40960 in outer maps to 1048576+40960 = 1089536
			outer:    []Extent{{Logical: 0, Physical: 40960, Length: 4096}},
			inner:    []Extent{{Logical: 0, Physical: 1048576, Length: 1048576}},
			expected: []Extent{{Logical: 0, Physical: 1089536, Length: 4096}},
		},
		{
			name: "gap in inner extents",
			// outer: logical [0,100) -> inner logical [50,150)
			// inner: logical [0,75) -> physical [1000,1075) (gap at 75-100), logical [100,200) -> physical [2000,2100)
			// The portion [50,75) maps to physical [1050,1075), [75,100) is a gap (skipped), [100,150) maps to [2000,2050)
			outer: []Extent{{Logical: 0, Physical: 50, Length: 100}},
			inner: []Extent{
				{Logical: 0, Physical: 1000, Length: 75},
				{Logical: 100, Physical: 2000, Length: 100},
			},
			expected: []Extent{
				{Logical: 0, Physical: 1050, Length: 25},
				{Logical: 50, Physical: 2000, Length: 50},
			},
		},
		{
			name:     "empty outer",
			outer:    []Extent{},
			inner:    []Extent{{Logical: 0, Physical: 1000, Length: 100}},
			expected: nil,
		},
		{
			name:     "empty inner",
			outer:    []Extent{{Logical: 0, Physical: 0, Length: 100}},
			inner:    []Extent{},
			expected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ComposeExtents(tt.outer, tt.inner)

			// Handle nil vs empty slice comparison
			if len(result) == 0 && len(tt.expected) == 0 {
				return
			}

			if !reflect.DeepEqual(result, tt.expected) {
				t.Errorf("ComposeExtents() =\n%v\nwant:\n%v", result, tt.expected)
			}
		})
	}
}

func TestExtentReaderAtFlattening(t *testing.T) {
	// Create base data: 1000 bytes
	baseData := make([]byte, 1000)
	for i := range baseData {
		baseData[i] = byte(i % 256)
	}
	baseReader := bytes.NewReader(baseData)

	// Inner ExtentReaderAt: maps [0,500) -> [100,600) in base
	innerExtents := []Extent{{Logical: 0, Physical: 100, Length: 500}}
	inner := NewExtentReaderAt(baseReader, innerExtents, 500)

	// Outer ExtentReaderAt wrapping inner: maps [0,200) -> [50,250) in inner
	// Which should compose to: [0,200) -> [150,350) in base
	outerExtents := []Extent{{Logical: 0, Physical: 50, Length: 200}}
	outer := NewExtentReaderAt(inner, outerExtents, 200)

	// Verify the outer reader uses the base reader directly (flattened)
	if outer.r != baseReader {
		t.Error("Expected outer to use baseReader directly after flattening")
	}

	// Verify composed extents
	if len(outer.extents) != 1 {
		t.Fatalf("Expected 1 composed extent, got %d", len(outer.extents))
	}
	if outer.extents[0].Logical != 0 || outer.extents[0].Physical != 150 || outer.extents[0].Length != 200 {
		t.Errorf("Unexpected composed extent: %+v", outer.extents[0])
	}

	// Verify we can read correct data
	buf := make([]byte, 10)
	n, err := outer.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt error: %v", err)
	}
	if n != 10 {
		t.Fatalf("Expected to read 10 bytes, got %d", n)
	}

	// Data at base offset 150-159 should be bytes 150-159
	for i := 0; i < 10; i++ {
		expected := byte((150 + i) % 256)
		if buf[i] != expected {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], expected)
		}
	}
}

func TestExtentReaderAtDeepNesting(t *testing.T) {
	// Simulate 4 levels of nesting, verify it flattens to direct base access
	baseData := make([]byte, 10000)
	for i := range baseData {
		baseData[i] = byte(i % 256)
	}
	baseReader := bytes.NewReader(baseData)

	// Level 1: [0, 5000) -> [1000, 6000)
	level1 := NewExtentReaderAt(baseReader, []Extent{{Logical: 0, Physical: 1000, Length: 5000}}, 5000)

	// Level 2: [0, 2000) -> [500, 2500) in level1 = [1500, 3500) in base
	level2 := NewExtentReaderAt(level1, []Extent{{Logical: 0, Physical: 500, Length: 2000}}, 2000)

	// Level 3: [0, 1000) -> [100, 1100) in level2 = [1600, 2600) in base
	level3 := NewExtentReaderAt(level2, []Extent{{Logical: 0, Physical: 100, Length: 1000}}, 1000)

	// Level 4: [0, 500) -> [50, 550) in level3 = [1650, 2150) in base
	level4 := NewExtentReaderAt(level3, []Extent{{Logical: 0, Physical: 50, Length: 500}}, 500)

	// Verify all levels point to base reader
	if level4.r != baseReader {
		t.Error("level4 should use baseReader directly")
	}
	if level3.r != baseReader {
		t.Error("level3 should use baseReader directly")
	}
	if level2.r != baseReader {
		t.Error("level2 should use baseReader directly")
	}

	// Verify final composed extent
	if len(level4.extents) != 1 {
		t.Fatalf("Expected 1 extent, got %d", len(level4.extents))
	}
	if level4.extents[0].Physical != 1650 {
		t.Errorf("Expected physical offset 1650, got %d", level4.extents[0].Physical)
	}

	// Read and verify data
	buf := make([]byte, 10)
	_, err := level4.ReadAt(buf, 0)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadAt error: %v", err)
	}

	for i := 0; i < 10; i++ {
		expected := byte((1650 + i) % 256)
		if buf[i] != expected {
			t.Errorf("buf[%d] = %d, want %d", i, buf[i], expected)
		}
	}
}

// bytesBuffer implements io.WriterAt for testing
type bytesBuffer struct {
	data []byte
}

func (b *bytesBuffer) WriteAt(p []byte, off int64) (int, error) {
	if int(off)+len(p) > len(b.data) {
		return 0, io.ErrShortWrite
	}
	copy(b.data[off:], p)
	return len(p), nil
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

func TestExtentWriterAt(t *testing.T) {
	// Create base buffer: 1000 bytes
	baseData := make([]byte, 1000)
	base := &bytesBuffer{data: baseData}

	// Create extent mapping: logical [0,200) -> physical [100,300)
	extents := []Extent{{Logical: 0, Physical: 100, Length: 200}}
	writer := NewExtentWriterAt(base, extents, 200)

	// Write "Hello" at logical offset 0 (physical 100)
	n, err := writer.WriteAt([]byte("Hello"), 0)
	if err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	if n != 5 {
		t.Errorf("Expected to write 5 bytes, got %d", n)
	}

	// Verify it was written at physical offset 100
	if string(baseData[100:105]) != "Hello" {
		t.Errorf("Expected 'Hello' at offset 100, got %q", baseData[100:105])
	}

	// Write at logical offset 50 (physical 150)
	n, err = writer.WriteAt([]byte("World"), 50)
	if err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	if string(baseData[150:155]) != "World" {
		t.Errorf("Expected 'World' at offset 150, got %q", baseData[150:155])
	}
}

func TestExtentWriterAtMultipleExtents(t *testing.T) {
	// Create base buffer
	baseData := make([]byte, 1000)
	base := &bytesBuffer{data: baseData}

	// Two extents: logical [0,100) -> physical [200,300), logical [100,200) -> physical [500,600)
	extents := []Extent{
		{Logical: 0, Physical: 200, Length: 100},
		{Logical: 100, Physical: 500, Length: 100},
	}
	writer := NewExtentWriterAt(base, extents, 200)

	// Write across extent boundary (90 bytes from offset 50)
	// logical [50,140) spans [50,100) in first extent and [100,140) in second
	data := make([]byte, 90)
	for i := range data {
		data[i] = byte(i + 1)
	}

	n, err := writer.WriteAt(data, 50)
	if err != nil {
		t.Fatalf("WriteAt error: %v", err)
	}
	if n != 90 {
		t.Errorf("Expected to write 90 bytes, got %d", n)
	}

	// Verify first part: logical [50,100) -> physical [250,300)
	for i := 0; i < 50; i++ {
		expected := byte(i + 1)
		if baseData[250+i] != expected {
			t.Errorf("baseData[%d] = %d, want %d", 250+i, baseData[250+i], expected)
		}
	}

	// Verify second part: logical [100,140) -> physical [500,540)
	for i := 0; i < 40; i++ {
		expected := byte(50 + i + 1)
		if baseData[500+i] != expected {
			t.Errorf("baseData[%d] = %d, want %d", 500+i, baseData[500+i], expected)
		}
	}
}

func TestExtentWriterAtBorrowFromReader(t *testing.T) {
	// Test that we can borrow extents from an ExtentReaderAt
	baseData := make([]byte, 1000)
	for i := range baseData {
		baseData[i] = byte(i % 256)
	}
	base := &bytesBuffer{data: baseData}

	// Create reader with extents
	extents := []Extent{{Logical: 0, Physical: 100, Length: 200}}
	reader := NewExtentReaderAt(base, extents, 200)

	// Borrow extents for writer
	writer := NewExtentWriterAt(base, reader.Extents(), reader.Size())

	// Write via writer
	writer.WriteAt([]byte("TEST"), 10)

	// Read back via reader
	buf := make([]byte, 4)
	reader.ReadAt(buf, 10)

	if string(buf) != "TEST" {
		t.Errorf("Expected 'TEST', got %q", buf)
	}

	// Also verify at physical location
	if string(baseData[110:114]) != "TEST" {
		t.Errorf("Expected 'TEST' at physical offset 110, got %q", baseData[110:114])
	}
}
