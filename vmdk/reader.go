// Package vmdk implements a reader for VMware Virtual Machine Disk (VMDK)
// sparse and stream-optimized extent files. It supports random read access
// over compressed (deflate/zlib) and uncompressed sparse extents without
// expanding the entire disk into memory.
//
// On-disk layout reference (sparse extent):
//
//	Offset 0:           SparseHeader  (512 bytes)
//	DescriptorOffset:   Embedded text descriptor
//	GDOffset:           Grain Directory (array of uint32 sector offsets)
//	GT offsets:         Grain Tables   (arrays of uint32 sector offsets)
//	Grain offsets:      Raw or compressed grain data
//
// For stream-optimized VMDKs (common in OVA exports), the grain directory
// and grain tables are at the END of the file, referenced by a footer that
// is a copy of the sparse header with corrected offsets.
package vmdk

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

const (
	sectorSize = 512
	vmdkMagic  = 0x564D444B // "VMDK" as little-endian uint32

	// Special grain table entry values
	gteUnallocated = 0
	gteZeroFill    = 1

	// Sparse header flags
	flagValidNewLineDetection = 1 << 0
	flagRedundantGrainTable   = 1 << 1
	flagZeroGrainGTE          = 1 << 2  // GTE value 1 = zero grain
	flagCompressedGrains      = 1 << 16
	flagHasMarkers            = 1 << 17

	// Compression algorithms
	compressNone    = 0
	compressDeflate = 1

	// Sentinel value indicating the real GD offset is in the footer.
	gdAtEndOfStream = 0xFFFFFFFFFFFFFFFF
)

// SparseHeader represents the 512-byte header at the start of a VMDK
// sparse extent (and the footer copy for stream-optimized extents).
//
// Field layout (little-endian, packed — some uint64 fields are NOT
// naturally aligned):
//
//	 0  uint32  magicNumber
//	 4  uint32  version
//	 8  uint32  flags
//	12  uint64  capacity          (virtual size in sectors)
//	20  uint64  grainSize         (sectors per grain)
//	28  uint64  descriptorOffset  (sectors)
//	36  uint64  descriptorSize    (sectors)
//	44  uint32  numGTEsPerGT      (entries per grain table, typically 512)
//	48  uint64  rgdOffset         (redundant grain directory, sectors)
//	56  uint64  gdOffset          (grain directory, sectors)
//	64  uint64  overHead          (metadata overhead, sectors)
//	72  uint8   uncleanShutdown
//	73  [4]byte line-ending detection bytes
//	77  uint16  compressAlgorithm
//	79  [433]byte padding
type SparseHeader struct {
	MagicNumber       uint32
	Version           uint32
	Flags             uint32
	Capacity          uint64 // virtual disk size in sectors
	GrainSize         uint64 // grain size in sectors (typically 128 = 64 KiB)
	DescriptorOffset  uint64
	DescriptorSize    uint64
	NumGTEsPerGT      uint32 // grain table entries per grain table (typically 512)
	RGDOffset         uint64
	GDOffset          uint64
	OverHead          uint64
	UncleanShutdown   byte
	CompressAlgorithm uint16
}

func parseSparseHeader(buf []byte) (*SparseHeader, error) {
	if len(buf) < sectorSize {
		return nil, fmt.Errorf("vmdk: header buffer too short (%d bytes)", len(buf))
	}
	le := binary.LittleEndian
	h := &SparseHeader{
		MagicNumber:       le.Uint32(buf[0:4]),
		Version:           le.Uint32(buf[4:8]),
		Flags:             le.Uint32(buf[8:12]),
		Capacity:          le.Uint64(buf[12:20]),
		GrainSize:         le.Uint64(buf[20:28]),
		DescriptorOffset:  le.Uint64(buf[28:36]),
		DescriptorSize:    le.Uint64(buf[36:44]),
		NumGTEsPerGT:      le.Uint32(buf[44:48]),
		RGDOffset:         le.Uint64(buf[48:56]),
		GDOffset:          le.Uint64(buf[56:64]),
		OverHead:          le.Uint64(buf[64:72]),
		UncleanShutdown:   buf[72],
		CompressAlgorithm: le.Uint16(buf[77:79]),
	}
	if h.MagicNumber != vmdkMagic {
		return nil, fmt.Errorf("vmdk: bad magic 0x%08X (expected 0x%08X)", h.MagicNumber, vmdkMagic)
	}
	return h, nil
}

// Reader provides random-access reads over a VMDK sparse extent.
// It implements io.ReaderAt over the virtual disk address space.
type Reader struct {
	r    io.ReaderAt
	size int64 // underlying extent file size

	header     *SparseHeader
	compressed bool

	grainSizeBytes uint64 // grain size in bytes
	numGDEntries   uint32 // number of grain directory entries

	// Grain directory: maps GDE index -> sector offset of grain table
	grainDir []uint32

	// Lazily-loaded grain tables, keyed by GDE index.
	grainTables map[uint32][]uint32
	mu          sync.Mutex
}

// Open creates a VMDK Reader from the given io.ReaderAt.
// size must be the total size of the underlying VMDK extent in bytes.
func Open(r io.ReaderAt, size int64) (*Reader, error) {
	// --- Read and validate primary header ---
	hdrBuf := make([]byte, sectorSize)
	if _, err := r.ReadAt(hdrBuf, 0); err != nil {
		return nil, fmt.Errorf("vmdk: reading primary header: %w", err)
	}
	header, err := parseSparseHeader(hdrBuf)
	if err != nil {
		return nil, err
	}

	if header.Version < 1 || header.Version > 3 {
		return nil, fmt.Errorf("vmdk: unsupported version %d", header.Version)
	}
	if header.GrainSize == 0 {
		return nil, fmt.Errorf("vmdk: grain size is 0")
	}
	if header.NumGTEsPerGT == 0 {
		return nil, fmt.Errorf("vmdk: numGTEsPerGT is 0")
	}

	compressed := header.Flags&flagCompressedGrains != 0
	if compressed && header.CompressAlgorithm != compressDeflate {
		return nil, fmt.Errorf("vmdk: unsupported compression algorithm %d", header.CompressAlgorithm)
	}

	// --- For stream-optimized extents the header's GDOffset is a
	// sentinel; the real offset lives in the footer at the end of file. ---
	if header.GDOffset == gdAtEndOfStream {
		footer, err := findFooter(r, size)
		if err != nil {
			return nil, fmt.Errorf("vmdk: locating stream-optimized footer: %w", err)
		}
		header.GDOffset = footer.GDOffset
		header.RGDOffset = footer.RGDOffset
		// Trust the footer's capacity if it differs (shouldn't, but be safe).
		if footer.Capacity > 0 {
			header.Capacity = footer.Capacity
		}
	}
	if header.GDOffset == 0 || header.GDOffset == gdAtEndOfStream {
		return nil, fmt.Errorf("vmdk: grain directory offset is invalid")
	}

	rd := &Reader{
		r:              r,
		size:           size,
		header:         header,
		compressed:     compressed,
		grainSizeBytes: header.GrainSize * sectorSize,
		grainTables:    make(map[uint32][]uint32),
	}

	// Number of grain directory entries = ceil(capacity / grains_per_GDE)
	grainsPerGDE := uint64(header.NumGTEsPerGT)
	totalGrains := (header.Capacity + header.GrainSize - 1) / header.GrainSize
	rd.numGDEntries = uint32((totalGrains + grainsPerGDE - 1) / grainsPerGDE)

	if err := rd.readGrainDirectory(); err != nil {
		return nil, err
	}
	return rd, nil
}

// findFooter scans known positions near the end of a stream-optimized VMDK
// to locate the footer (a copy of the sparse header with real offsets).
func findFooter(r io.ReaderAt, size int64) (*SparseHeader, error) {
	buf := make([]byte, sectorSize)
	// The file typically ends with:
	//   ... [footer marker sector] [footer data (512 B)] [EOS marker sector]
	// Try several candidate positions.
	for _, off := range []int64{
		size - 2*sectorSize,  // most common: second-to-last sector
		size - 3*sectorSize,  // footer marker position
		size - sectorSize,    // last sector (some producers)
	} {
		if off < 0 {
			continue
		}
		if _, err := r.ReadAt(buf, off); err != nil {
			continue
		}
		if binary.LittleEndian.Uint32(buf[:4]) != vmdkMagic {
			continue
		}
		hdr, err := parseSparseHeader(buf)
		if err != nil {
			continue
		}
		// The footer must have a usable GD offset.
		if hdr.GDOffset != 0 && hdr.GDOffset != gdAtEndOfStream {
			return hdr, nil
		}
	}
	return nil, fmt.Errorf("could not find a valid footer with VMDK magic")
}

// readGrainDirectory reads the grain directory from disk.
// Each entry is a uint32 sector offset pointing to a grain table.
func (rd *Reader) readGrainDirectory() error {
	n := int(rd.numGDEntries)
	buf := make([]byte, n*4)
	gdOff := int64(rd.header.GDOffset) * sectorSize
	if _, err := rd.r.ReadAt(buf, gdOff); err != nil {
		return fmt.Errorf("vmdk: reading grain directory (%d entries at offset %d): %w", n, gdOff, err)
	}
	rd.grainDir = make([]uint32, n)
	for i := 0; i < n; i++ {
		rd.grainDir[i] = binary.LittleEndian.Uint32(buf[i*4 : i*4+4])
	}
	return nil
}

// loadGrainTable loads (and caches) the grain table for the given GDE index.
func (rd *Reader) loadGrainTable(gdeIdx uint32) ([]uint32, error) {
	rd.mu.Lock()
	defer rd.mu.Unlock()
	if gt, ok := rd.grainTables[gdeIdx]; ok {
		return gt, nil
	}

	n := int(rd.header.NumGTEsPerGT)
	buf := make([]byte, n*4)
	gtOff := int64(rd.grainDir[gdeIdx]) * sectorSize
	if _, err := rd.r.ReadAt(buf, gtOff); err != nil {
		return nil, fmt.Errorf("vmdk: reading grain table %d at offset %d: %w", gdeIdx, gtOff, err)
	}
	gt := make([]uint32, n)
	for i := 0; i < n; i++ {
		gt[i] = binary.LittleEndian.Uint32(buf[i*4 : i*4+4])
	}
	rd.grainTables[gdeIdx] = gt
	return gt, nil
}

// VirtualSize returns the virtual disk capacity in bytes.
func (rd *Reader) VirtualSize() uint64 {
	return rd.header.Capacity * sectorSize
}

// Header returns the parsed sparse header (or the footer copy for
// stream-optimized extents, with corrected offsets).
func (rd *Reader) Header() *SparseHeader { return rd.header }

// IsCompressed reports whether grains are deflate-compressed.
func (rd *Reader) IsCompressed() bool { return rd.compressed }

// Descriptor reads and returns the embedded VMDK descriptor text.
func (rd *Reader) Descriptor() (string, error) {
	off := rd.header.DescriptorOffset
	sz := rd.header.DescriptorSize
	if off == 0 || sz == 0 {
		return "", nil
	}
	buf := make([]byte, sz*sectorSize)
	if _, err := rd.r.ReadAt(buf, int64(off)*sectorSize); err != nil {
		return "", fmt.Errorf("vmdk: reading descriptor: %w", err)
	}
	// Descriptor is NUL-terminated text.
	if i := bytes.IndexByte(buf, 0); i >= 0 {
		buf = buf[:i]
	}
	return string(buf), nil
}

// ReadAt reads len(p) bytes from the virtual disk starting at byte offset off.
// Unallocated (sparse) regions return zeroes. ReadAt is safe for concurrent use.
func (rd *Reader) ReadAt(p []byte, off int64) (int, error) {
	vsize := int64(rd.VirtualSize())
	if off >= vsize {
		return 0, io.EOF
	}

	total := 0
	for len(p) > 0 && off < vsize {
		// Which grain does this offset fall into?
		grainIdx := uint64(off) / rd.grainSizeBytes
		grainOff := uint64(off) % rd.grainSizeBytes // offset within grain

		// Bytes available in this grain from the current offset.
		avail := rd.grainSizeBytes - grainOff
		want := uint64(len(p))
		if want > avail {
			want = avail
		}
		if rem := uint64(vsize - off); want > rem {
			want = rem
		}

		// Resolve GDE and GTE indices.
		gtesPerGT := uint64(rd.header.NumGTEsPerGT)
		gdeIdx := uint32(grainIdx / gtesPerGT)
		gteIdx := uint32(grainIdx % gtesPerGT)

		if gdeIdx >= rd.numGDEntries || rd.grainDir[gdeIdx] == 0 {
			// Entire grain table is unallocated — zero fill.
			zeroFill(p[:want])
		} else {
			gt, err := rd.loadGrainTable(gdeIdx)
			if err != nil {
				return total, err
			}
			gte := gt[gteIdx]
			if gte == gteUnallocated || gte == gteZeroFill {
				zeroFill(p[:want])
			} else {
				grain, err := rd.readGrain(gte)
				if err != nil {
					return total, fmt.Errorf("vmdk: grain GDE=%d GTE=%d: %w", gdeIdx, gteIdx, err)
				}
				// Ensure we don't overread a short grain (shouldn't happen).
				end := grainOff + want
				if end > uint64(len(grain)) {
					end = uint64(len(grain))
					zeroFill(p[:want])
				}
				copy(p[:want], grain[grainOff:end])
			}
		}

		p = p[want:]
		off += int64(want)
		total += int(want)
	}

	if total == 0 {
		return 0, io.EOF
	}
	return total, nil
}

// readGrain reads and returns the full decompressed (or raw) grain data
// for the grain whose GTE value is gte (a sector offset).
func (rd *Reader) readGrain(gte uint32) ([]byte, error) {
	offset := int64(gte) * sectorSize
	if rd.compressed {
		return rd.readCompressedGrain(offset)
	}
	return rd.readRawGrain(offset)
}

func (rd *Reader) readRawGrain(offset int64) ([]byte, error) {
	buf := make([]byte, rd.grainSizeBytes)
	n, err := rd.r.ReadAt(buf, offset)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("reading raw grain at 0x%x: %w", offset, err)
	}
	return buf[:n], nil
}

// readCompressedGrain reads a compressed grain marker and decompresses it.
//
// Stream-optimized grain marker layout:
//
//	Bytes 0-7:   LBA   (uint64 LE — starting sector of this grain)
//	Bytes 8-11:  size  (uint32 LE — compressed byte count)
//	Bytes 12+:   compressed data (size bytes)
//	Padded to 512-byte boundary.
func (rd *Reader) readCompressedGrain(offset int64) ([]byte, error) {
	// Read the 12-byte marker header.
	var mhdr [12]byte
	if _, err := rd.r.ReadAt(mhdr[:], offset); err != nil {
		return nil, fmt.Errorf("reading grain marker at 0x%x: %w", offset, err)
	}
	compSize := binary.LittleEndian.Uint32(mhdr[8:12])
	if compSize == 0 {
		// Zero-length compressed data → zero-filled grain.
		return make([]byte, rd.grainSizeBytes), nil
	}

	// Read compressed payload.
	comp := make([]byte, compSize)
	if _, err := rd.r.ReadAt(comp, offset+12); err != nil {
		return nil, fmt.Errorf("reading %d compressed bytes at 0x%x: %w", compSize, offset+12, err)
	}

	return rd.decompress(comp)
}

// decompress tries zlib (used by VMware) then falls back to raw deflate.
func (rd *Reader) decompress(data []byte) ([]byte, error) {
	buf := make([]byte, rd.grainSizeBytes)

	// Attempt 1: zlib (RFC 1950) — VMware's typical choice.
	if zr, err := zlib.NewReader(bytes.NewReader(data)); err == nil {
		n, readErr := io.ReadFull(zr, buf)
		zr.Close()
		if readErr == nil || readErr == io.ErrUnexpectedEOF {
			// Zero-fill any trailing portion (already zero from make).
			_ = n
			return buf, nil
		}
	}

	// Attempt 2: raw deflate (RFC 1951).
	fr := flate.NewReader(bytes.NewReader(data))
	n, err := io.ReadFull(fr, buf)
	fr.Close()
	if err == nil || err == io.ErrUnexpectedEOF {
		_ = n
		return buf, nil
	}

	return nil, fmt.Errorf("decompression failed (tried zlib and raw deflate): %w", err)
}

func zeroFill(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
