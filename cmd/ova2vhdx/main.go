// Command ova2vhdx converts an OVA (Open Virtual Appliance) file into
// VHDX disk images compatible with Microsoft Hyper-V.
//
// By default it converts ALL embedded VMDK disks. For multi-disk OVAs the
// output files are named with a "-diskN" suffix (e.g. disk-disk0.vhdx,
// disk-disk1.vhdx). Use --disk to convert a single disk.
//
// Usage:
//
//	ova2vhdx --input appliance.ova --output disk.vhdx [--verbose] [--progress] [--disk N]
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ova2vhdx/ova"
	"ova2vhdx/ovf"
	"ova2vhdx/vhdx"
	"ova2vhdx/vmdk"
)

// Set via -ldflags at build time.
var (
	version = "dev"
	commit  = "none"
)

func main() {
	var (
		input      string
		output     string
		verbose    bool
		progress   bool
		diskIdx    int
		showVer    bool
	)
	flag.StringVar(&input, "input", "", "Path to input .ova file (required)")
	flag.StringVar(&output, "output", "", "Path to output .vhdx file (required)")
	flag.BoolVar(&verbose, "verbose", false, "Enable verbose logging")
	flag.BoolVar(&progress, "progress", false, "Show progress during conversion")
	flag.IntVar(&diskIdx, "disk", -1, "Convert only the disk at this zero-based index (default: all disks)")
	flag.BoolVar(&showVer, "version", false, "Print version and exit")
	flag.Parse()

	if showVer {
		fmt.Printf("ova2vhdx %s (%s)\n", version, commit)
		return
	}

	if input == "" || output == "" {
		fmt.Fprintln(os.Stderr, "Usage: ova2vhdx --input <file.ova> --output <file.vhdx>")
		flag.PrintDefaults()
		os.Exit(1)
	}

	logger := log.New(os.Stderr, "", log.LstdFlags)
	if !verbose {
		logger.SetOutput(io.Discard)
	}

	if err := run(input, output, diskIdx, verbose, progress, logger); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run(input, output string, diskIdx int, verbose, progress bool, logger *log.Logger) error {
	totalStart := time.Now()

	// ---- 1. Open and index the OVA archive ----
	logger.Printf("Opening OVA: %s", input)
	archive, err := ova.Open(input)
	if err != nil {
		return err
	}
	defer archive.Close()

	fmt.Fprintf(os.Stderr, "OVA contains %d disk(s):\n", len(archive.Disks))
	for i, d := range archive.Disks {
		fmt.Fprintf(os.Stderr, "  [%d] %s  (%d MiB compressed)\n", i, d.Name, d.Size/(1024*1024))
	}

	// ---- 2. Parse the OVF descriptor (informational) ----
	desc, err := ovf.Parse(archive.OVF)
	if err != nil {
		logger.Printf("WARNING: OVF parse error (continuing): %v", err)
	} else {
		for i, d := range desc.Disks {
			cap := d.CapacityBytes()
			logger.Printf("  OVF disk[%d]: id=%s  capacity=%d bytes (%.1f GiB)  format=%s",
				i, d.DiskID, cap, float64(cap)/(1024*1024*1024), d.Format)
		}
		for _, f := range desc.Files {
			logger.Printf("  OVF file: id=%s  href=%s  size=%d", f.ID, f.Href, f.Size)
		}
	}

	// ---- 3. Determine which disks to convert ----
	var indices []int
	if diskIdx >= 0 {
		if diskIdx >= len(archive.Disks) {
			return fmt.Errorf("disk index %d out of range (OVA contains %d disk(s))", diskIdx, len(archive.Disks))
		}
		indices = []int{diskIdx}
	} else {
		for i := range archive.Disks {
			indices = append(indices, i)
		}
	}

	// ---- 4. Convert each disk ----
	for _, idx := range indices {
		disk := archive.Disks[idx]
		outPath := outputPath(output, idx, len(archive.Disks))

		fmt.Fprintf(os.Stderr, "\n--- Disk %d/%d: %s -> %s ---\n", idx+1, len(archive.Disks), disk.Name, filepath.Base(outPath))

		if err := convertDisk(archive, disk, outPath, verbose, progress, logger); err != nil {
			return fmt.Errorf("disk %d (%s): %w", idx, disk.Name, err)
		}
	}

	fmt.Fprintf(os.Stderr, "\nAll done in %s.\n", time.Since(totalStart).Round(time.Millisecond))
	return nil
}

// outputPath derives the output file path for a given disk index.
// For single-disk OVAs (or --disk mode) it returns the path as-is.
// For multi-disk OVAs it inserts "-diskN" before the extension.
func outputPath(base string, idx, total int) string {
	if total <= 1 {
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return fmt.Sprintf("%s-disk%d%s", stem, idx, ext)
}

func convertDisk(archive *ova.Archive, disk ova.DiskEntry, outPath string, verbose, progress bool, logger *log.Logger) error {
	start := time.Now()

	// ---- Open VMDK ----
	sr := archive.NewDiskReader(disk)
	vmdkReader, err := vmdk.Open(sr, disk.Size)
	if err != nil {
		return fmt.Errorf("opening VMDK %q: %w", disk.Name, err)
	}

	virtualSize := vmdkReader.VirtualSize()
	fmt.Fprintf(os.Stderr, "  Virtual size: %.1f GiB  |  Compressed: %v  |  Grain: %d KiB\n",
		float64(virtualSize)/(1024*1024*1024), vmdkReader.IsCompressed(),
		vmdkReader.Header().GrainSize*512/1024)

	if verbose {
		if descText, err := vmdkReader.Descriptor(); err == nil && descText != "" {
			logger.Printf("VMDK descriptor:\n%s", indent(descText))
		}
	}

	// ---- Create VHDX ----
	vhdxWriter, err := vhdx.Create(outPath, virtualSize)
	if err != nil {
		return fmt.Errorf("creating VHDX: %w", err)
	}

	// ---- Stream blocks ----
	blockSize := int64(vhdxWriter.BlockSize())
	numBlocks := vhdxWriter.DataBlockCount()
	buf := make([]byte, blockSize)

	var written, skipped uint32
	lastReport := time.Now()

	for i := uint32(0); i < numBlocks; i++ {
		off := int64(i) * blockSize
		readSize := blockSize
		if rem := int64(virtualSize) - off; rem < readSize {
			readSize = rem
		}

		for j := range buf {
			buf[j] = 0
		}

		if _, err := vmdkReader.ReadAt(buf[:readSize], off); err != nil && err != io.EOF {
			return fmt.Errorf("reading block %d at offset 0x%x: %w", i, off, err)
		}

		nonZero, err := vhdxWriter.WriteBlock(i, buf)
		if err != nil {
			return fmt.Errorf("writing block %d: %w", i, err)
		}
		if nonZero {
			written++
		} else {
			skipped++
		}

		if progress && time.Since(lastReport) > 200*time.Millisecond {
			pct := float64(i+1) / float64(numBlocks) * 100
			fmt.Fprintf(os.Stderr, "\r  Converting: %d/%d blocks (%.1f%%)  [%d written, %d zero-skipped]",
				i+1, numBlocks, pct, written, skipped)
			lastReport = time.Now()
		}
	}

	if progress {
		fmt.Fprintf(os.Stderr, "\r  Converting: %d/%d blocks (100.0%%)  [%d written, %d zero-skipped]\n",
			numBlocks, numBlocks, written, skipped)
	}

	// ---- Finalize ----
	if err := vhdxWriter.Close(); err != nil {
		return fmt.Errorf("finalizing VHDX: %w", err)
	}

	elapsed := time.Since(start)
	dataMB := float64(written) * float64(blockSize) / (1024 * 1024)
	fmt.Fprintf(os.Stderr, "  Done in %s. Wrote %.0f MiB across %d blocks (%d zero-skipped). Output: %s\n",
		elapsed.Round(time.Millisecond), dataMB, written, skipped, outPath)
	return nil
}

func indent(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = "    " + lines[i]
	}
	return strings.Join(lines, "\n")
}
