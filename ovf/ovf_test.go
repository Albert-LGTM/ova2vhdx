package ovf

import (
	"testing"
)

const sampleOVF = `<?xml version="1.0" encoding="UTF-8"?>
<Envelope xmlns="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:ovf="http://schemas.dmtf.org/ovf/envelope/1"
          xmlns:vssd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_VirtualSystemSettingData"
          xmlns:rasd="http://schemas.dmtf.org/wbem/wscim/1/cim-schema/2/CIM_ResourceAllocationSettingData">
  <References>
    <File ovf:id="file1" ovf:href="TestVM-disk1.vmdk" ovf:size="525139968"/>
    <File ovf:id="file2" ovf:href="TestVM-disk2.vmdk" ovf:size="102400"/>
  </References>
  <DiskSection>
    <Info>Virtual disk information</Info>
    <Disk ovf:capacity="40" ovf:capacityAllocationUnits="byte * 2^30"
          ovf:diskId="vmdisk1" ovf:fileRef="file1"
          ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
    <Disk ovf:capacity="1024" ovf:capacityAllocationUnits="byte * 2^20"
          ovf:diskId="vmdisk2" ovf:fileRef="file2"
          ovf:format="http://www.vmware.com/interfaces/specifications/vmdk.html#streamOptimized"/>
  </DiskSection>
  <VirtualSystem ovf:id="TestVM">
    <Info>A virtual machine</Info>
  </VirtualSystem>
</Envelope>`

func TestParse(t *testing.T) {
	desc, err := Parse(sampleOVF)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(desc.Files) != 2 {
		t.Fatalf("got %d files, want 2", len(desc.Files))
	}
	if desc.Files[0].Href != "TestVM-disk1.vmdk" {
		t.Errorf("file[0].Href = %q", desc.Files[0].Href)
	}
	if desc.Files[0].Size != 525139968 {
		t.Errorf("file[0].Size = %d", desc.Files[0].Size)
	}
	if desc.Files[1].ID != "file2" {
		t.Errorf("file[1].ID = %q", desc.Files[1].ID)
	}

	if len(desc.Disks) != 2 {
		t.Fatalf("got %d disks, want 2", len(desc.Disks))
	}
	if desc.Disks[0].DiskID != "vmdisk1" {
		t.Errorf("disk[0].DiskID = %q", desc.Disks[0].DiskID)
	}
	if desc.Disks[0].FileRef != "file1" {
		t.Errorf("disk[0].FileRef = %q", desc.Disks[0].FileRef)
	}
}

func TestCapacityBytes(t *testing.T) {
	desc, _ := Parse(sampleOVF)
	if len(desc.Disks) < 2 {
		t.Fatal("not enough disks")
	}

	// Disk 0: 40 * 2^30 = 40 GiB
	got := desc.Disks[0].CapacityBytes()
	want := uint64(40) * 1024 * 1024 * 1024
	if got != want {
		t.Errorf("disk[0] capacity = %d, want %d", got, want)
	}

	// Disk 1: 1024 * 2^20 = 1 GiB
	got = desc.Disks[1].CapacityBytes()
	want = uint64(1024) * 1024 * 1024
	if got != want {
		t.Errorf("disk[1] capacity = %d, want %d", got, want)
	}
}

func TestApplyUnits(t *testing.T) {
	tests := []struct {
		value uint64
		units string
		want  uint64
	}{
		{1, "byte * 2^30", 1 << 30},
		{1, "byte * 2^20", 1 << 20},
		{1, "byte * 2^10", 1 << 10},
		{1, "byte * 2^9", 512},
		{1024, "", 1024},
		{1024, "byte", 1024},
	}
	for _, tt := range tests {
		got := applyUnits(tt.value, tt.units)
		if got != tt.want {
			t.Errorf("applyUnits(%d, %q) = %d, want %d", tt.value, tt.units, got, tt.want)
		}
	}
}

// Test that the parser handles OVF without namespaces (some older formats).
func TestParseNoNamespace(t *testing.T) {
	ovf := `<?xml version="1.0"?>
<Envelope>
  <References>
    <File id="f1" href="disk.vmdk" size="100"/>
  </References>
  <DiskSection>
    <Disk diskId="d1" capacity="1073741824" fileRef="f1"/>
  </DiskSection>
</Envelope>`

	desc, err := Parse(ovf)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(desc.Files) != 1 || desc.Files[0].Href != "disk.vmdk" {
		t.Errorf("unexpected files: %+v", desc.Files)
	}
	if len(desc.Disks) != 1 || desc.Disks[0].DiskID != "d1" {
		t.Errorf("unexpected disks: %+v", desc.Disks)
	}
	// No units → raw bytes.
	if desc.Disks[0].CapacityBytes() != 1073741824 {
		t.Errorf("capacity = %d, want 1073741824", desc.Disks[0].CapacityBytes())
	}
}
