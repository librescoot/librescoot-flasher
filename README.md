# librescoot-flasher

Fast firmware flasher with bmap support for LibreScoot scooters.

## Features

- Sparse writes via bmap (only mapped blocks written)
- Two-phase safe flash (partitions first, boot area last)
- Gzip decompression on-the-fly
- SHA256 verification of bmap ranges
- Machine-readable progress on stderr

## Usage

**Bmap mode** (fastest, writes only mapped blocks):
```
librescoot-flasher --image firmware.sdimg.gz --bmap firmware.bmap --device /dev/sdb
```

**Two-phase mode** (safe: partitions written before boot sector):
```
librescoot-flasher --image firmware.sdimg.gz --device /dev/sdb --two-phase
```

**Sequential mode** (full image write, no bmap):
```
librescoot-flasher --image firmware.sdimg --device /dev/sdb
```

### Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--image` | required | Firmware image (`.sdimg` or `.sdimg.gz`) |
| `--device` | required | Target block device |
| `--bmap` | — | Bmap file path (enables sparse writes) |
| `--two-phase` | false | Write partitions first, boot area last |
| `--boot-blocks` | 6 | Boot area size in 4 MB blocks (two-phase only) |
| `--version` | — | Print version and exit |

## Progress output

Progress is reported on stderr:

```
TOTAL:<bytes>       # emitted once at start (compressed size in sequential/bmap mode)
PROGRESS:<bytes>    # emitted at most once per second (bytes written so far)
DONE                # emitted on success
ERROR: <message>    # emitted on failure
```

Two-phase mode also emits `PHASE:A` and `PHASE:B` markers.

## Build

```
go build .
```

Or use the Makefile:

```
make          # all platforms
make linux    # linux/amd64 + linux/arm64
make arm      # linux/arm (armv7)
make darwin   # darwin/arm64
make windows  # windows/amd64
make clean
```

## License

AGPL-3.0 — see [LICENSE](LICENSE).
