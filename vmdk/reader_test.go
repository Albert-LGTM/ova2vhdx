package vmdk

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"io"
	"testing"
)

// buildSparseVMDK constructs a minimal valid monolithicSparse VMDK in memory
// with one grain containing known data and the rest sparse.
func buildSparseVMDK(t *testing.T, grainData []byte) []byte {
	t.Helper()

	const (
		grainSizeSectors = 128               // 64 KiB grains
		grainSizeBytes   = grainSizeSectors * sectorSize
		numGTEsPerGT     = 512
		capacity         = grainSizeSectors * numGTEsPerGT // 1 GT worth of grains
	)

	if len(grainData) > grainSizeBytes {
		t.Fatal("grainData exceeds grain size")
	}

	// Layout (all offsets in sectors):
	//   sector 0:    sparse header
	//   sector 1-2:  descriptor (1024 bytes)
	//   sector 3:    grain directory (1 entry = 4 bytes, padded to sector)
	//   sector 4-7:  grain table (512 entries × 4 bytes = 2048 bytes = 4 sectors)
	//   sector 8+:   grain 0 data (128 sectors)

	descOff := uint64(1)
	descSize := uint64(2)
	gdOff := uint64(3)
	gtOff := uint64(4)
	grainOff := uint64(8) // first grain at sector 8

	totalSectors := grainOff + grainSizeSectors
	buf := make([]byte, totalSectors*sectorSize)
	le := binary.LittleEndian

	// --- Sparse header ---
	le.PutUint32(buf[0:4], vmdkMagic)
	le.PutUint32(buf[4:8], 1)                    // version
	le.PutUint32(buf[8:12], 0)                    // flags (no compression)
	le.PutUint64(buf[12:20], capacity)            // capacity in sectors
	le.PutUint64(buf[20:28], grainSizeSectors)    // grain size
	le.PutUint64(buf[28:36], descOff)             // descriptorOffset
	le.PutUint64(buf[36:44], descSize)            // descriptorSize
	le.PutUint32(buf[44:48], numGTEsPerGT)        // numGTEsPerGT
	le.PutUint64(buf[48:56], 0)                   // rgdOffset
	le.PutUint64(buf[56:64], gdOff)               // gdOffset
	le.PutUint64(buf[64:72], grainOff)            // overHead
	buf[72] = 0                                    // uncleanShutdown
	le.PutUint16(buf[77:79], compressNone)         // compressAlgorithm

	// --- Descriptor ---
	desc := []byte("# Disk DescriptorFile\nversion=1\ncreateType=\"monolithicSparse\"\n")
	copy(buf[descOff*sectorSize:], desc)

	// --- Grain directory (1 entry pointing to the GT) ---
	le.PutUint32(buf[gdOff*sectorSize:], uint32(gtOff))

	// --- Grain table (entry 0 -> grain at grainOff, rest = 0 = sparse) ---
	le.PutUint32(buf[gtOff*sectorSize:], uint32(grainOff))

	// --- Grain data ---
	grain := buf[grainOff*sectorSize : (grainOff+grainSizeSectors)*sectorSize]
	copy(grain, grainData)

	return buf
}

func TestOpenAndReadSparse(t *testing.T) {
	// Fill grain 0 with a recognizable pattern.
	grainSize := 128 * sectorSize // 64 KiB
	pattern := make([]byte, grainSize)
	for i := range pattern {
		pattern[i] = byte(i % 251) // prime-period pattern
	}

	img := buildSparseVMDK(t, pattern)
	r, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Virtual size should be 128 * 512 * 512 = 32 MiB.
	expectedVSize := uint64(128 * 512 * sectorSize)
	if r.VirtualSize() != expectedVSize {
		t.Fatalf("VirtualSize = %d, want %d", r.VirtualSize(), expectedVSize)
	}

	// Read grain 0 and verify pattern.
	buf := make([]byte, grainSize)
	n, err := r.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt grain 0: %v", err)
	}
	if n != grainSize {
		t.Fatalf("ReadAt returned %d bytes, want %d", n, grainSize)
	}
	if !bytes.Equal(buf, pattern) {
		t.Error("grain 0 data mismatch")
	}

	// Read grain 1 (sparse) — should be all zeros.
	n, err = r.ReadAt(buf, int64(grainSize))
	if err != nil {
		t.Fatalf("ReadAt grain 1: %v", err)
	}
	for i, b := range buf[:n] {
		if b != 0 {
			t.Fatalf("grain 1 byte %d = 0x%02x, want 0x00", i, b)
		}
	}
}

func TestReadAtSpansGrains(t *testing.T) {
	grainSize := 128 * sectorSize
	pattern := bytes.Repeat([]byte{0xAB}, grainSize)
	img := buildSparseVMDK(t, pattern)
	r, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Read across grain 0 / grain 1 boundary.
	cross := make([]byte, 1024)
	off := int64(grainSize - 512) // 512 bytes before end of grain 0
	n, err := r.ReadAt(cross, off)
	if err != nil {
		t.Fatalf("ReadAt cross-grain: %v", err)
	}
	if n != 1024 {
		t.Fatalf("cross-grain read = %d bytes, want 1024", n)
	}
	// First 512 bytes from grain 0 (0xAB), next 512 from grain 1 (0x00).
	for i := 0; i < 512; i++ {
		if cross[i] != 0xAB {
			t.Fatalf("byte %d = 0x%02x, want 0xAB", i, cross[i])
		}
	}
	for i := 512; i < 1024; i++ {
		if cross[i] != 0x00 {
			t.Fatalf("byte %d = 0x%02x, want 0x00", i, cross[i])
		}
	}
}

func TestReadAtEOF(t *testing.T) {
	img := buildSparseVMDK(t, nil)
	r, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	buf := make([]byte, 512)
	_, err = r.ReadAt(buf, int64(r.VirtualSize()))
	if err != io.EOF {
		t.Fatalf("expected io.EOF at virtual size, got %v", err)
	}
}

func TestDescriptor(t *testing.T) {
	img := buildSparseVMDK(t, nil)
	r, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	desc, err := r.Descriptor()
	if err != nil {
		t.Fatalf("Descriptor: %v", err)
	}
	if !bytes.Contains([]byte(desc), []byte("monolithicSparse")) {
		t.Errorf("descriptor missing createType, got: %q", desc)
	}
}

// buildStreamOptVMDK creates a minimal stream-optimized VMDK with one
// zlib-compressed grain.
func buildStreamOptVMDK(t *testing.T, grainData []byte) []byte {
	t.Helper()

	const (
		grainSizeSectors = 128
		grainSizeBytes   = grainSizeSectors * sectorSize
		numGTEsPerGT     = 512
		capacity         = grainSizeSectors * numGTEsPerGT
	)

	if len(grainData) == 0 {
		grainData = make([]byte, grainSizeBytes)
		for i := range grainData {
			grainData[i] = byte(i % 199)
		}
	}

	// Compress the grain with zlib.
	var compBuf bytes.Buffer
	zw := zlib.NewWriter(&compBuf)
	zw.Write(grainData)
	zw.Close()
	compData := compBuf.Bytes()

	// Grain marker: LBA(8) + compSize(4) + compData, padded to sector.
	markerSize := 12 + len(compData)
	markerSectors := (markerSize + sectorSize - 1) / sectorSize

	// Layout (sectors):
	//   0:           primary header (GDOffset = sentinel)
	//   1-2:         descriptor
	//   3+:          compressed grain marker
	//   after grain: GT marker region (grain table, 4 sectors)
	//   after GT:    GD (1 sector)
	//   after GD:    footer (1 sector)
	//   last:        EOS marker (1 sector)

	descOff := uint64(1)
	descSize := uint64(2)
	grainMarkerOff := uint64(3)
	gtOff := grainMarkerOff + uint64(markerSectors)
	gdOff := gtOff + 4 // GT is 512*4=2048 bytes = 4 sectors
	footerOff := gdOff + 1
	eosOff := footerOff + 1
	totalSectors := eosOff + 1

	buf := make([]byte, totalSectors*sectorSize)
	le := binary.LittleEndian

	// --- Primary header (GDOffset = sentinel) ---
	le.PutUint32(buf[0:4], vmdkMagic)
	le.PutUint32(buf[4:8], 3) // version 3
	le.PutUint32(buf[8:12], flagCompressedGrains|flagHasMarkers)
	le.PutUint64(buf[12:20], capacity)
	le.PutUint64(buf[20:28], grainSizeSectors)
	le.PutUint64(buf[28:36], descOff)
	le.PutUint64(buf[36:44], descSize)
	le.PutUint32(buf[44:48], numGTEsPerGT)
	le.PutUint64(buf[48:56], 0) // rgdOffset
	le.PutUint64(buf[56:64], gdAtEndOfStream)
	le.PutUint64(buf[64:72], grainMarkerOff) // overHead
	le.PutUint16(buf[77:79], compressDeflate)

	// --- Descriptor ---
	desc := []byte("# Disk DescriptorFile\nversion=1\ncreateType=\"streamOptimized\"\n")
	copy(buf[descOff*sectorSize:], desc)

	// --- Compressed grain marker at grainMarkerOff ---
	gmOff := grainMarkerOff * sectorSize
	le.PutUint64(buf[gmOff:gmOff+8], 0) // LBA = 0
	le.PutUint32(buf[gmOff+8:gmOff+12], uint32(len(compData)))
	copy(buf[gmOff+12:], compData)

	// --- Grain table (entry 0 -> grainMarkerOff, rest sparse) ---
	le.PutUint32(buf[gtOff*sectorSize:], uint32(grainMarkerOff))

	// --- Grain directory (1 entry -> gtOff) ---
	le.PutUint32(buf[gdOff*sectorSize:], uint32(gtOff))

	// --- Footer (copy of header with corrected GDOffset) ---
	fOff := footerOff * sectorSize
	copy(buf[fOff:fOff+sectorSize], buf[0:sectorSize])
	le.PutUint64(buf[fOff+56:fOff+64], gdOff) // patch GDOffset

	// --- EOS marker (all zeros is fine; marker with type=0) ---
	// Already zero.

	return buf
}

func TestStreamOptimizedVMDK(t *testing.T) {
	grainSize := 128 * sectorSize
	pattern := make([]byte, grainSize)
	for i := range pattern {
		pattern[i] = byte(i % 199)
	}

	img := buildStreamOptVMDK(t, pattern)
	r, err := Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatalf("Open stream-optimized: %v", err)
	}

	if !r.IsCompressed() {
		t.Fatal("expected compressed=true for stream-optimized VMDK")
	}

	buf := make([]byte, grainSize)
	n, err := r.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != grainSize {
		t.Fatalf("ReadAt returned %d bytes, want %d", n, grainSize)
	}
	if !bytes.Equal(buf, pattern) {
		t.Error("stream-optimized grain 0 data mismatch")
	}
}

func TestBadMagic(t *testing.T) {
	buf := make([]byte, sectorSize)
	binary.LittleEndian.PutUint32(buf[0:4], 0xDEADBEEF)
	_, err := Open(bytes.NewReader(buf), int64(len(buf)))
	if err == nil {
		t.Fatal("expected error for bad magic")
	}
}
