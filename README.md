# ova2vhdx

A pure-Go CLI tool that converts OVA (Open Virtual Appliance) files into VHDX disk images compatible with Microsoft Hyper-V. No external dependencies like `qemu-img` or `VBoxManage` required — it ships as a single static binary.

## Features

- Converts stream-optimized and sparse VMDK disks to dynamic VHDX
- Produces Hyper-V compatible VHDX with correct headers, metadata, BAT, and CRC32C checksums
- Streams data in 32 MiB chunks — handles multi-GB disks without loading them into memory
- Skips zero-filled blocks to keep the output VHDX sparse
- Reads VMDK data directly from the OVA tar archive (no temp file extraction)
- Multi-disk OVA support — converts all disks by default
- Cross-platform — single binary for Windows, Linux, or macOS

## Installation

### Download a release

Grab a prebuilt binary from the [Releases](../../releases) page.

### Build from source

Requires Go 1.25+.

```bash
# Native build
make build

# Or directly
go build -o ova2vhdx ./cmd/ova2vhdx
```

### Build release binaries for all platforms

```bash
make release
```

This produces binaries in `dist/` for:
- `windows/amd64`, `windows/arm64`
- `linux/amd64`, `linux/arm64`
- `darwin/amd64`, `darwin/arm64`

## Usage

```
ova2vhdx --input <file.ova> --output <file.vhdx> [options]
```

### Options

| Flag | Description |
|---|---|
| `--input` | Path to the input `.ova` file (required) |
| `--output` | Path to the output `.vhdx` file (required) |
| `--verbose` | Print detailed logs (VMDK geometry, OVF metadata, descriptor) |
| `--progress` | Show a live progress indicator during conversion |
| `--disk N` | Convert only the Nth disk (zero-based). Default: all disks |
| `--version` | Print version and exit |

### Examples

Convert all disks in an OVA:

```bash
ova2vhdx --input myvm.ova --output myvm.vhdx --progress
```

For multi-disk OVAs this produces `myvm-disk0.vhdx`, `myvm-disk1.vhdx`, etc.

Convert a single specific disk:

```bash
ova2vhdx --input myvm.ova --output boot.vhdx --disk 0
```

### Importing into Hyper-V

1. Run the conversion to produce `.vhdx` file(s)
2. Open **Hyper-V Manager**
3. Create a new VM or edit an existing one
4. Under **IDE Controller** or **SCSI Controller**, click **Add Hard Drive**
5. Select **Virtual hard disk** and browse to the `.vhdx` file
6. Start the VM

## Project structure

```
cmd/ova2vhdx/   CLI entry point and conversion pipeline
ova/            OVA (tar) extraction with offset tracking for zero-copy disk access
ovf/            OVF XML descriptor parser (namespace-agnostic)
vmdk/           VMDK sparse and stream-optimized extent reader
vhdx/           VHDX dynamic disk writer
```

## How it works

```
OVA (tar) ──extract──> OVF descriptor ──parse──> disk references
                   └──> VMDK extent ──random access via SectionReader──┐
                                                                       v
                                                              VMDKReader.ReadAt()
                                                              (grain dir → grain table → decompress)
                                                                       │
                                                              32 MiB block loop
                                                                       │
                                                                       v
                                                              VHDXWriter.WriteBlock()
                                                              (BAT entry + data block at 1 MiB alignment)
                                                                       │
                                                                       v
                                                              Finalize BAT ──> .vhdx
```

**VMDK Reader** parses the sparse header (or locates the footer for stream-optimized extents), loads grain directories and grain tables on demand, and decompresses grains (zlib/deflate). It exposes an `io.ReaderAt` interface over the full virtual disk address space — unallocated regions return zeroes.

**VHDX Writer** produces a spec-compliant dynamic VHDX: file type identifier, dual headers with CRC32C, dual region tables, a metadata region with all 5 required items (file parameters, virtual disk size, page 83 data, logical/physical sector sizes), and a BAT interleaved with sector-bitmap placeholders.

## Running tests

```bash
make test
```

## Releasing

Push a version tag to trigger the GitHub Actions release workflow:

```bash
git tag v1.0.0
git push origin v1.0.0
```

This automatically:
1. Runs tests
2. Builds binaries for all 6 platform/arch combinations
3. Generates SHA-256 checksums
4. Creates a GitHub Release with the binaries attached

## Limitations

- Supports sparse and stream-optimized VMDK extents only (covers the vast majority of OVA exports from VMware, VirtualBox, and similar tools)
- Does not support split VMDK (`-s` flag extents across multiple files)
- Does not support VMDK flat/raw extents (monolithicFlat, vmfs)
- VHDX output is always a dynamic disk (no fixed or differencing support)
- Compressed grains must use deflate (VMDK compression algorithm 1)

## Contributing

1. Fork the repository
2. Create a feature branch (`git checkout -b feature/my-change`)
3. Run tests (`make test`)
4. Commit your changes
5. Open a pull request

## License

[MIT](LICENSE)
