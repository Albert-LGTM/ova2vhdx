package ova

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// buildTestOVA creates a minimal OVA (tar) file with an OVF and a fake VMDK.
func buildTestOVA(t *testing.T, dir string) string {
	t.Helper()

	ovaPath := filepath.Join(dir, "test.ova")
	f, err := os.Create(ovaPath)
	if err != nil {
		t.Fatal(err)
	}
	tw := tar.NewWriter(f)

	// Write OVF entry.
	ovfContent := []byte(`<?xml version="1.0"?>
<Envelope>
  <References><File id="f1" href="disk.vmdk" size="1024"/></References>
  <DiskSection><Disk diskId="d1" capacity="1048576" fileRef="f1"/></DiskSection>
</Envelope>`)
	tw.WriteHeader(&tar.Header{Name: "test.ovf", Size: int64(len(ovfContent))})
	tw.Write(ovfContent)

	// Write a fake VMDK entry (just bytes, not a real VMDK).
	vmdkContent := bytes.Repeat([]byte{0xAA}, 2048)
	tw.WriteHeader(&tar.Header{Name: "disk.vmdk", Size: int64(len(vmdkContent))})
	tw.Write(vmdkContent)

	tw.Close()
	f.Close()
	return ovaPath
}

func TestOpen(t *testing.T) {
	dir := t.TempDir()
	ovaPath := buildTestOVA(t, dir)

	a, err := Open(ovaPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	if a.OVF == "" {
		t.Error("OVF content is empty")
	}
	if len(a.Disks) != 1 {
		t.Fatalf("got %d disks, want 1", len(a.Disks))
	}
	if a.Disks[0].Name != "disk.vmdk" {
		t.Errorf("disk name = %q", a.Disks[0].Name)
	}
	if a.Disks[0].Size != 2048 {
		t.Errorf("disk size = %d, want 2048", a.Disks[0].Size)
	}
}

func TestNewDiskReader(t *testing.T) {
	dir := t.TempDir()
	ovaPath := buildTestOVA(t, dir)

	a, err := Open(ovaPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer a.Close()

	sr := a.NewDiskReader(a.Disks[0])

	// Read first few bytes — should be 0xAA (our fake VMDK content).
	buf := make([]byte, 16)
	n, err := sr.ReadAt(buf, 0)
	if err != nil {
		t.Fatalf("ReadAt: %v", err)
	}
	if n != 16 {
		t.Fatalf("ReadAt returned %d bytes", n)
	}
	for i, b := range buf {
		if b != 0xAA {
			t.Fatalf("byte %d = 0x%02x, want 0xAA", i, b)
		}
	}
}

func TestOpenMissingOVF(t *testing.T) {
	dir := t.TempDir()
	ovaPath := filepath.Join(dir, "bad.ova")
	f, _ := os.Create(ovaPath)
	tw := tar.NewWriter(f)
	tw.WriteHeader(&tar.Header{Name: "readme.txt", Size: 5})
	tw.Write([]byte("hello"))
	tw.Close()
	f.Close()

	_, err := Open(ovaPath)
	if err == nil {
		t.Fatal("expected error for OVA without .ovf")
	}
}

func TestOpenMissingVMDK(t *testing.T) {
	dir := t.TempDir()
	ovaPath := filepath.Join(dir, "bad.ova")
	f, _ := os.Create(ovaPath)
	tw := tar.NewWriter(f)
	ovf := []byte("<Envelope/>")
	tw.WriteHeader(&tar.Header{Name: "vm.ovf", Size: int64(len(ovf))})
	tw.Write(ovf)
	tw.Close()
	f.Close()

	_, err := Open(ovaPath)
	if err == nil {
		t.Fatal("expected error for OVA without .vmdk")
	}
}
