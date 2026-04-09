// Package vhdx implements a writer for Microsoft VHDX dynamic disk images
// compatible with Hyper-V. It produces a valid VHDX file from scratch,
// writing proper file identifiers, headers, region tables, metadata,
// a Block Allocation Table (BAT), and data blocks.
//
// VHDX file layout produced by this writer:
//
//	0x000000  File Type Identifier   (64 KiB)
//	0x010000  Header 1               (4 KiB payload, 64 KiB region)
//	0x020000  Header 2               (4 KiB payload, 64 KiB region)
//	0x030000  Region Table 1         (64 KiB)
//	0x040000  Region Table 2         (64 KiB)
//	0x100000  Log Region             (1 MiB, empty)
//	0x200000  Metadata Region        (1 MiB)
//	0x300000  BAT Region             (variable, 1 MiB aligned)
//	...       Data Blocks            (blockSize each, 1 MiB aligned)
//
// Reference: MS-VHDX specification.
package vhdx

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
)

// ------------------------------------------------------------------ constants

const (
	kib = 1024
	mib = 1024 * kib

	fileIDOffset     int64 = 0x000000
	header1Offset    int64 = 0x010000
	header2Offset    int64 = 0x020000
	regTable1Offset  int64 = 0x030000
	regTable2Offset  int64 = 0x040000
	logRegionOffset  int64 = 0x100000
	logRegionSize    int64 = 1 * mib
	metadataOffset   int64 = 0x200000
	metadataSize     int64 = 1 * mib
	batStartOffset   int64 = 0x300000

	defaultBlockSize       uint32 = 32 * mib
	defaultLogicalSector   uint32 = 512
	defaultPhysicalSector  uint32 = 4096

	// Signatures (little-endian encoding of ASCII strings).
	sigFileID    uint64 = 0x656C696678646876 // "vhdxfile"
	sigHeader    uint32 = 0x64616568         // "head"
	sigRegion    uint32 = 0x69676572         // "regi"
	sigMetadata  uint64 = 0x617461646174656D // "metadata"

	// BAT payload block states (bits 0-2 of each 64-bit entry).
	batNotPresent    uint64 = 0
	batFullyPresent  uint64 = 6
)

// GUIDs used in region table entries and metadata items (mixed-endian).
var (
	guidBAT      = mustGUID("2DC27766-F623-4200-9D64-115E9BFD4A08")
	guidMetadata = mustGUID("8B7CA206-4790-4B9A-B8FE-575F050F886E")

	guidFileParameters   = mustGUID("CAA16737-FA36-4D43-B3B6-33F0AA44E76B")
	guidVirtualDiskSize  = mustGUID("2FA54224-CD1B-4876-B211-5DBED83BF4B8")
	guidPage83Data       = mustGUID("BECA12AB-B2E6-4523-93EF-C309E000C746")
	guidLogicalSector    = mustGUID("8141BF1D-A96F-4709-BA47-F233A8FAAB5F")
	guidPhysicalSector   = mustGUID("CDA348C7-445D-4471-9CC9-E9885251C556")
)

// -------------------------------------------------------------------- GUID helpers

type guid [16]byte

// mustGUID parses "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX" into mixed-endian bytes.
func mustGUID(s string) guid {
	var d1 uint32
	var d2, d3 uint16
	var d4a uint16
	var d4b uint64
	_, err := fmt.Sscanf(s, "%08X-%04X-%04X-%04X-%012X", &d1, &d2, &d3, &d4a, &d4b)
	if err != nil {
		panic("bad GUID literal: " + s)
	}
	var g guid
	binary.LittleEndian.PutUint32(g[0:4], d1)
	binary.LittleEndian.PutUint16(g[4:6], d2)
	binary.LittleEndian.PutUint16(g[6:8], d3)
	g[8] = byte(d4a >> 8)
	g[9] = byte(d4a)
	g[10] = byte(d4b >> 40)
	g[11] = byte(d4b >> 32)
	g[12] = byte(d4b >> 24)
	g[13] = byte(d4b >> 16)
	g[14] = byte(d4b >> 8)
	g[15] = byte(d4b)
	return g
}

func randomGUID() guid {
	var g guid
	_, _ = rand.Read(g[:])
	g[7] = (g[7] & 0x0F) | 0x40 // version 4
	g[8] = (g[8] & 0x3F) | 0x80 // variant 2
	return g
}

// -------------------------------------------------------- CRC-32C (Castagnoli)

var crc32cTab = crc32.MakeTable(crc32.Castagnoli)

func crc32c(b []byte) uint32 { return crc32.Checksum(b, crc32cTab) }

// ---------------------------------------------------------------- Writer

// Writer creates a dynamic VHDX disk image.
type Writer struct {
	f *os.File

	virtualSize    uint64
	blockSize      uint32
	logicalSector  uint32
	physicalSector uint32

	dataBlockCount  uint32
	chunkRatio      uint32
	totalBATEntries uint32
	bat             []uint64

	batFileOffset   int64
	dataStartOffset int64
	nextDataOffset  int64

	fileWriteGUID guid
	dataWriteGUID guid
	diskID        guid
}

// Create opens path for writing and initializes all VHDX metadata structures.
// The caller must eventually call Close to flush the BAT and finalize the file.
func Create(path string, virtualSize uint64) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, fmt.Errorf("vhdx: %w", err)
	}

	w := &Writer{
		f:              f,
		virtualSize:    virtualSize,
		blockSize:      defaultBlockSize,
		logicalSector:  defaultLogicalSector,
		physicalSector: defaultPhysicalSector,
		fileWriteGUID:  randomGUID(),
		dataWriteGUID:  randomGUID(),
		diskID:         randomGUID(),
	}

	// --- BAT geometry ---
	w.dataBlockCount = uint32((virtualSize + uint64(w.blockSize) - 1) / uint64(w.blockSize))
	// chunk_ratio = (2^23 * logical_sector_size) / block_size
	w.chunkRatio = uint32((uint64(1<<23) * uint64(w.logicalSector)) / uint64(w.blockSize))

	// Total BAT entries: payload blocks interleaved with sector-bitmap slots.
	// For dynamic disks the SB entries are NOT_PRESENT but must be reserved.
	sbEntries := uint32(0)
	if w.dataBlockCount > 1 {
		sbEntries = (w.dataBlockCount - 1) / w.chunkRatio
	}
	w.totalBATEntries = w.dataBlockCount + sbEntries
	w.bat = make([]uint64, w.totalBATEntries)

	// BAT region size rounded up to 1 MiB.
	batBytes := int64(w.totalBATEntries) * 8
	batAligned := alignUp(batBytes, mib)

	w.batFileOffset = batStartOffset
	w.dataStartOffset = alignUp(batStartOffset+batAligned, mib)
	w.nextDataOffset = w.dataStartOffset

	// --- Write all structural regions ---
	for _, fn := range []func() error{
		w.writeFileIdentifier,
		w.writeHeaders,
		func() error { return w.writeRegionTables(batAligned) },
		w.writeMetadata,
	} {
		if err := fn(); err != nil {
			f.Close()
			return nil, err
		}
	}

	return w, nil
}

// BlockSize returns the data-block size (bytes).
func (w *Writer) BlockSize() uint32 { return w.blockSize }

// DataBlockCount returns the number of payload blocks that cover the virtual disk.
func (w *Writer) DataBlockCount() uint32 { return w.dataBlockCount }

// WriteBlock writes a full data block for block index blockIdx.
// data must be exactly blockSize bytes (the caller pads the last block).
// Returns true if the block contained non-zero data and was persisted.
func (w *Writer) WriteBlock(blockIdx uint32, data []byte) (bool, error) {
	if isZero(data) {
		return false, nil // leave BAT entry as NOT_PRESENT
	}

	off := w.nextDataOffset

	// Pad to full block size if needed (last block).
	if len(data) < int(w.blockSize) {
		padded := make([]byte, w.blockSize)
		copy(padded, data)
		data = padded
	}

	if _, err := w.f.WriteAt(data, off); err != nil {
		return false, fmt.Errorf("vhdx: writing block %d at 0x%x: %w", blockIdx, off, err)
	}

	// BAT entry: top 44 bits = file offset (naturally 1 MiB aligned),
	// bottom 3 bits = state.
	batIdx := w.payloadBATIndex(blockIdx)
	w.bat[batIdx] = (uint64(off) & 0xFFFFFFFFFFF00000) | batFullyPresent

	w.nextDataOffset += int64(w.blockSize)
	return true, nil
}

// Close writes the final BAT to disk and closes the file.
func (w *Writer) Close() error {
	// Serialize BAT.
	buf := make([]byte, len(w.bat)*8)
	for i, e := range w.bat {
		binary.LittleEndian.PutUint64(buf[i*8:i*8+8], e)
	}
	if _, err := w.f.WriteAt(buf, w.batFileOffset); err != nil {
		return fmt.Errorf("vhdx: writing BAT: %w", err)
	}
	return w.f.Close()
}

// -------------------------------------------------- payload BAT index mapping

// payloadBATIndex maps a logical block index to the BAT array position,
// accounting for interleaved sector-bitmap entries.
//
// Layout: [PB×chunkRatio] [SB] [PB×chunkRatio] [SB] ... [PB×remainder]
func (w *Writer) payloadBATIndex(blockIdx uint32) uint32 {
	chunk := blockIdx / w.chunkRatio
	rem := blockIdx % w.chunkRatio
	return chunk*(w.chunkRatio+1) + rem
}

// -------------------------------------------------- structural writers

func (w *Writer) writeFileIdentifier() error {
	buf := make([]byte, 64*kib)
	binary.LittleEndian.PutUint64(buf[0:8], sigFileID)
	// Creator string in UTF-16LE.
	const creator = "ova2vhdx"
	for i, ch := range creator {
		binary.LittleEndian.PutUint16(buf[8+i*2:10+i*2], uint16(ch))
	}
	_, err := w.f.WriteAt(buf, fileIDOffset)
	return err
}

func (w *Writer) writeHeaders() error {
	for _, cfg := range []struct {
		off int64
		seq uint64
	}{
		{header1Offset, 1},
		{header2Offset, 2},
	} {
		hdr := make([]byte, 4*kib)
		le := binary.LittleEndian
		le.PutUint32(hdr[0:4], sigHeader)
		// Checksum placeholder at 4:8
		le.PutUint64(hdr[8:16], cfg.seq)
		copy(hdr[16:32], w.fileWriteGUID[:])
		copy(hdr[32:48], w.dataWriteGUID[:])
		// LogGuid at 48:64 — all zero (no active log)
		le.PutUint16(hdr[64:66], 0) // LogVersion
		le.PutUint16(hdr[66:68], 1) // Version
		le.PutUint32(hdr[68:72], uint32(logRegionSize))
		le.PutUint64(hdr[72:80], uint64(logRegionOffset))
		// Checksum over 4 KiB header.
		le.PutUint32(hdr[4:8], 0)
		le.PutUint32(hdr[4:8], crc32c(hdr))
		if _, err := w.f.WriteAt(hdr, cfg.off); err != nil {
			return fmt.Errorf("vhdx: writing header at 0x%x: %w", cfg.off, err)
		}
	}
	return nil
}

func (w *Writer) writeRegionTables(batSize int64) error {
	buf := make([]byte, 64*kib)
	le := binary.LittleEndian

	le.PutUint32(buf[0:4], sigRegion)
	// Checksum at 4:8 (later)
	le.PutUint32(buf[8:12], 2)  // EntryCount
	le.PutUint32(buf[12:16], 0) // Reserved

	// Entry 1: BAT  (offset 16, 32 bytes)
	off := 16
	copy(buf[off:off+16], guidBAT[:])
	le.PutUint64(buf[off+16:off+24], uint64(w.batFileOffset))
	le.PutUint32(buf[off+24:off+28], uint32(batSize))
	le.PutUint32(buf[off+28:off+32], 1) // Required

	// Entry 2: Metadata  (offset 48, 32 bytes)
	off = 48
	copy(buf[off:off+16], guidMetadata[:])
	le.PutUint64(buf[off+16:off+24], uint64(metadataOffset))
	le.PutUint32(buf[off+24:off+28], uint32(metadataSize))
	le.PutUint32(buf[off+28:off+32], 1) // Required

	// Checksum over the full 64 KiB.
	le.PutUint32(buf[4:8], 0)
	le.PutUint32(buf[4:8], crc32c(buf))

	for _, o := range []int64{regTable1Offset, regTable2Offset} {
		if _, err := w.f.WriteAt(buf, o); err != nil {
			return fmt.Errorf("vhdx: writing region table at 0x%x: %w", o, err)
		}
	}
	return nil
}

// writeMetadata writes the metadata region: a table of 5 required items
// followed by their payloads.
//
// Metadata table layout (at start of metadata region):
//
//	 0  uint64  Signature ("metadata")
//	 8  uint16  Reserved
//	10  uint16  EntryCount
//	12  [20]byte Reserved
//	32  Entry[0..4]  (each 32 bytes)
//
// Each entry: GUID(16) Offset(4) Length(4) Flags(4) Reserved(4)
//
// Flags: bit 0 = IsUser, bit 1 = IsVirtualDisk, bit 2 = IsRequired
func (w *Writer) writeMetadata() error {
	buf := make([]byte, metadataSize)
	le := binary.LittleEndian

	// --- Table header ---
	le.PutUint64(buf[0:8], sigMetadata)
	le.PutUint16(buf[10:12], 5) // 5 entries

	// Item payloads start at 64 KiB within the metadata region,
	// each on a 4 KiB boundary.
	const itemBase = 64 * kib

	type mdEntry struct {
		id     guid
		off    uint32
		length uint32
		flags  uint32
	}
	entries := []mdEntry{
		{guidFileParameters, itemBase, 8, 0x04},           // IsRequired
		{guidVirtualDiskSize, itemBase + 4096, 8, 0x06},   // IsRequired | IsVirtualDisk
		{guidPage83Data, itemBase + 2*4096, 16, 0x06},     // IsRequired | IsVirtualDisk
		{guidLogicalSector, itemBase + 3*4096, 4, 0x06},   // IsRequired | IsVirtualDisk
		{guidPhysicalSector, itemBase + 4*4096, 4, 0x06},  // IsRequired | IsVirtualDisk
	}
	for i, e := range entries {
		o := 32 + i*32
		copy(buf[o:o+16], e.id[:])
		le.PutUint32(buf[o+16:o+20], e.off)
		le.PutUint32(buf[o+20:o+24], e.length)
		le.PutUint32(buf[o+24:o+28], e.flags)
	}

	// --- Item payloads ---
	// File Parameters: BlockSize(4) + Flags(4)
	le.PutUint32(buf[itemBase:itemBase+4], w.blockSize)
	le.PutUint32(buf[itemBase+4:itemBase+8], 0) // not a differencing disk

	// Virtual Disk Size
	le.PutUint64(buf[itemBase+4096:itemBase+4096+8], w.virtualSize)

	// Page 83 Data (virtual disk GUID)
	copy(buf[itemBase+2*4096:itemBase+2*4096+16], w.diskID[:])

	// Logical Sector Size
	le.PutUint32(buf[itemBase+3*4096:itemBase+3*4096+4], w.logicalSector)

	// Physical Sector Size
	le.PutUint32(buf[itemBase+4*4096:itemBase+4*4096+4], w.physicalSector)

	_, err := w.f.WriteAt(buf, metadataOffset)
	return err
}

// ------------------------------------------------------------------ helpers

func alignUp(v int64, align int64) int64 {
	return (v + align - 1) / align * align
}

func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
