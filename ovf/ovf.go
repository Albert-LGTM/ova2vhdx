// Package ovf provides a lenient parser for OVF (Open Virtualization Format)
// XML descriptors. It extracts disk and file references regardless of the
// OVF namespace version (VMware, DMTF 1.x, DMTF 2.x) by matching on
// local element/attribute names only.
package ovf

import (
	"encoding/xml"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// FileRef describes a <File> element in the OVF <References> section.
type FileRef struct {
	ID   string // ovf:id
	Href string // ovf:href (file name inside the OVA)
	Size int64  // ovf:size (bytes, may be 0 if omitted)
}

// DiskInfo describes a <Disk> element in the OVF <DiskSection>.
type DiskInfo struct {
	DiskID                  string // ovf:diskId
	Capacity                uint64 // ovf:capacity (raw numeric value)
	CapacityAllocationUnits string // ovf:capacityAllocationUnits (e.g. "byte * 2^30")
	FileRef                 string // ovf:fileRef (maps to FileRef.ID)
	Format                  string // ovf:format
}

// CapacityBytes returns the disk capacity in bytes, interpreting the
// allocation units string. Falls back to treating the raw capacity as bytes.
func (d *DiskInfo) CapacityBytes() uint64 {
	return applyUnits(d.Capacity, d.CapacityAllocationUnits)
}

// Descriptor holds the parsed contents of an OVF XML document.
type Descriptor struct {
	Files []FileRef
	Disks []DiskInfo
}

// Parse reads OVF XML from data and returns the parsed descriptor.
// It uses a token-based approach matching local names, so it works
// regardless of namespace prefixes or URIs.
func Parse(data string) (*Descriptor, error) {
	dec := xml.NewDecoder(strings.NewReader(data))
	desc := &Descriptor{}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("ovf: XML parse error: %w", err)
		}

		se, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}

		switch se.Name.Local {
		case "File":
			var f FileRef
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "id":
					f.ID = a.Value
				case "href":
					f.Href = a.Value
				case "size":
					f.Size, _ = strconv.ParseInt(a.Value, 10, 64)
				}
			}
			if f.Href != "" {
				desc.Files = append(desc.Files, f)
			}

		case "Disk":
			var d DiskInfo
			for _, a := range se.Attr {
				switch a.Name.Local {
				case "diskId":
					d.DiskID = a.Value
				case "capacity":
					d.Capacity, _ = strconv.ParseUint(a.Value, 10, 64)
				case "capacityAllocationUnits":
					d.CapacityAllocationUnits = a.Value
				case "fileRef":
					d.FileRef = a.Value
				case "format":
					d.Format = a.Value
				}
			}
			desc.Disks = append(desc.Disks, d)
		}
	}

	return desc, nil
}

// applyUnits interprets common OVF capacity allocation unit strings.
func applyUnits(value uint64, units string) uint64 {
	u := strings.ToLower(strings.TrimSpace(units))
	switch {
	case strings.Contains(u, "2^30"):
		return value * 1024 * 1024 * 1024
	case strings.Contains(u, "2^20"):
		return value * 1024 * 1024
	case strings.Contains(u, "2^10"):
		return value * 1024
	case strings.Contains(u, "2^9"):
		return value * 512
	default:
		return value // assume bytes
	}
}
