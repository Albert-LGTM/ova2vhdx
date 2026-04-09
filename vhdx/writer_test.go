package vhdx

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateAndClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	virtualSize := uint64(64 * 1024 * 1024) // 64 MiB
	w, err := Create(path, virtualSize)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if w.DataBlockCount() != 2 { // 64 MiB / 32 MiB = 2
		t.Errorf("DataBlockCount = %d, want 2", w.DataBlockCount())
	}

	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Verify the file exists and has non-zero size.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if fi.Size() == 0 {
		t.Error("VHDX file is empty")
	}
}

func TestFileIdentifier(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.Close()

	f, _ := os.Open(path)
	defer f.Close()

	var sig [8]byte
	f.ReadAt(sig[:], 0)
	got := binary.LittleEndian.Uint64(sig[:])
	if got != sigFileID {
		t.Errorf("file identifier signature = 0x%016x, want 0x%016x", got, sigFileID)
	}
}

func TestHeaderSignatures(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.Close()

	f, _ := os.Open(path)
	defer f.Close()

	for _, off := range []int64{header1Offset, header2Offset} {
		var sig [4]byte
		f.ReadAt(sig[:], off)
		got := binary.LittleEndian.Uint32(sig[:])
		if got != sigHeader {
			t.Errorf("header signature at 0x%x = 0x%08x, want 0x%08x", off, got, sigHeader)
		}
	}
}

func TestHeaderChecksums(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.Close()

	f, _ := os.Open(path)
	defer f.Close()

	for _, off := range []int64{header1Offset, header2Offset} {
		hdr := make([]byte, 4096)
		f.ReadAt(hdr, off)

		stored := binary.LittleEndian.Uint32(hdr[4:8])
		// Zero the checksum field and recompute.
		binary.LittleEndian.PutUint32(hdr[4:8], 0)
		computed := crc32c(hdr)
		if stored != computed {
			t.Errorf("header checksum at 0x%x: stored=0x%08x computed=0x%08x", off, stored, computed)
		}
	}
}

func TestRegionTableChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.Close()

	f, _ := os.Open(path)
	defer f.Close()

	for _, off := range []int64{regTable1Offset, regTable2Offset} {
		rt := make([]byte, 64*1024)
		f.ReadAt(rt, off)

		sig := binary.LittleEndian.Uint32(rt[0:4])
		if sig != sigRegion {
			t.Errorf("region table signature at 0x%x = 0x%08x, want 0x%08x", off, sig, sigRegion)
		}

		stored := binary.LittleEndian.Uint32(rt[4:8])
		binary.LittleEndian.PutUint32(rt[4:8], 0)
		computed := crc32c(rt)
		if stored != computed {
			t.Errorf("region table checksum at 0x%x: stored=0x%08x computed=0x%08x", off, stored, computed)
		}
	}
}

func TestMetadataSignature(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	w.Close()

	f, _ := os.Open(path)
	defer f.Close()

	var sig [8]byte
	f.ReadAt(sig[:], metadataOffset)
	got := binary.LittleEndian.Uint64(sig[:])
	if got != sigMetadata {
		t.Errorf("metadata signature = 0x%016x, want 0x%016x", got, sigMetadata)
	}
}

func TestWriteBlockZeroSkip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	zeros := make([]byte, w.BlockSize())
	written, err := w.WriteBlock(0, zeros)
	if err != nil {
		t.Fatalf("WriteBlock: %v", err)
	}
	if written {
		t.Error("expected zero block to be skipped")
	}
	w.Close()
}

func TestWriteBlockNonZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	data := make([]byte, w.BlockSize())
	data[0] = 0xFF
	written, err := w.WriteBlock(0, data)
	if err != nil {
		t.Fatalf("WriteBlock: %v", err)
	}
	if !written {
		t.Error("expected non-zero block to be written")
	}
	w.Close()
}

func TestPayloadBATIndex(t *testing.T) {
	// Use a writer with known geometry to test BAT index calculation.
	w := &Writer{
		chunkRatio: 128,
	}

	tests := []struct {
		blockIdx uint32
		want     uint32
	}{
		{0, 0},
		{127, 127},
		{128, 129},   // skip SB at index 128
		{255, 256},
		{256, 258},   // skip SBs at 128 and 257
	}
	for _, tt := range tests {
		got := w.payloadBATIndex(tt.blockIdx)
		if got != tt.want {
			t.Errorf("payloadBATIndex(%d) = %d, want %d", tt.blockIdx, got, tt.want)
		}
	}
}

func TestBATEntryFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.vhdx")

	w, err := Create(path, 64*1024*1024)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	data := make([]byte, w.BlockSize())
	data[42] = 1
	w.WriteBlock(0, data)
	w.Close()

	f, _ := os.Open(path)
	defer f.Close()

	var entry [8]byte
	f.ReadAt(entry[:], w.batFileOffset)
	val := binary.LittleEndian.Uint64(entry[:])

	state := val & 0x7
	if state != batFullyPresent {
		t.Errorf("BAT state = %d, want %d (FULLY_PRESENT)", state, batFullyPresent)
	}

	fileOff := val & 0xFFFFFFFFFFF00000
	if fileOff == 0 {
		t.Error("BAT file offset is zero for written block")
	}
	if fileOff%uint64(mib) != 0 {
		t.Errorf("BAT file offset 0x%x is not 1 MiB aligned", fileOff)
	}
}

func TestGUIDParsing(t *testing.T) {
	// Verify that our GUID parsing produces the expected mixed-endian layout.
	g := mustGUID("2DC27766-F623-4200-9D64-115E9BFD4A08")

	// Data1 = 0x2DC27766 in little-endian: 66 77 C2 2D
	if g[0] != 0x66 || g[1] != 0x77 || g[2] != 0xC2 || g[3] != 0x2D {
		t.Errorf("Data1 bytes = %x, want 6677C22D", g[0:4])
	}
	// Data2 = 0xF623 in little-endian: 23 F6
	if g[4] != 0x23 || g[5] != 0xF6 {
		t.Errorf("Data2 bytes = %x, want 23F6", g[4:6])
	}
	// Data4[0:2] = 9D 64 (big-endian)
	if g[8] != 0x9D || g[9] != 0x64 {
		t.Errorf("Data4[0:2] = %x, want 9D64", g[8:10])
	}
}
