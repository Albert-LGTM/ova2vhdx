// Package ova extracts and indexes the contents of an OVA (Open Virtual
// Appliance) archive, which is a standard TAR file containing an OVF
// descriptor and one or more VMDK disk images.
//
// The package uses a counting reader around archive/tar so that it can
// record the byte offsets of embedded VMDK files. After scanning, callers
// obtain an io.SectionReader for each disk, giving efficient random-access
// reads without extracting to a temporary file.
package ova

import (
	"archive/tar"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DiskEntry describes a VMDK disk found inside the OVA archive.
type DiskEntry struct {
	Name   string // base file name (e.g. "disk1.vmdk")
	Offset int64  // byte offset of the VMDK data within the OVA file
	Size   int64  // size in bytes
}

// Archive represents an opened OVA file whose structure has been indexed.
type Archive struct {
	file  *os.File
	OVF   string      // raw OVF XML content
	Disks []DiskEntry // VMDK disk entries (in archive order)
}

// Open opens the OVA file at path, scans its TAR entries to locate the
// OVF descriptor and any VMDK disks, and returns a ready-to-use Archive.
//
// The underlying file remains open until Close is called.
func Open(path string) (*Archive, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("ova: %w", err)
	}

	a := &Archive{file: f}

	// Wrap the file in a counting reader so we can track the absolute byte
	// offset as archive/tar consumes data.
	cr := &countingReader{r: f}
	tr := tar.NewReader(cr)

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("ova: reading tar: %w", err)
		}

		base := filepath.Base(hdr.Name)
		lower := strings.ToLower(base)

		switch {
		case strings.HasSuffix(lower, ".ovf"):
			raw, err := io.ReadAll(tr)
			if err != nil {
				f.Close()
				return nil, fmt.Errorf("ova: reading OVF entry %q: %w", hdr.Name, err)
			}
			a.OVF = string(raw)

		case strings.HasSuffix(lower, ".vmdk"):
			// cr.n is the cumulative byte count consumed from the file.
			// After tar.Next() returns, this is the first byte of entry data.
			a.Disks = append(a.Disks, DiskEntry{
				Name:   base,
				Offset: cr.n,
				Size:   hdr.Size,
			})
		}
	}

	if a.OVF == "" {
		f.Close()
		return nil, fmt.Errorf("ova: no .ovf descriptor found in archive")
	}
	if len(a.Disks) == 0 {
		f.Close()
		return nil, fmt.Errorf("ova: no .vmdk disk images found in archive")
	}
	return a, nil
}

// NewDiskReader returns an io.SectionReader that provides random-access
// reads over the raw VMDK bytes embedded in the OVA, without extraction.
func (a *Archive) NewDiskReader(d DiskEntry) *io.SectionReader {
	return io.NewSectionReader(a.file, d.Offset, d.Size)
}

// Close releases the underlying OVA file handle.
func (a *Archive) Close() error {
	return a.file.Close()
}

// countingReader wraps an io.Reader and tracks the total number of bytes
// consumed.  archive/tar performs all its I/O through this reader, so
// after each call to tar.Next() the counter reflects the file position
// at the start of the entry's data payload.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
