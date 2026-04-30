# librescoot-flasher

Fast firmware flasher with bmap support for Librescoot scooters.

Part of the [Librescoot](https://librescoot.org/) open-source platform.

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

This project is dual-licensed. The source code is available under the
[GNU Affero General Public License v3.0][agpl-3.0].
The maintainers reserve the right to grant separate licenses for commercial distribution; please contact the maintainers to discuss commercial licensing.

[![AGPL v3][agpl-image]][agpl-3.0]

[agpl-3.0]: https://www.gnu.org/licenses/agpl-3.0.en.html
[agpl-image]: https://www.gnu.org/graphics/agplv3-88x31.png
