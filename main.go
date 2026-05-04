package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"hash"
	"io"
	"os"
	"strconv"
	"strings"
	"time"
)

var version = "dev"

func main() {
	imagePath := flag.String("image", "", "Path to firmware image (.sdimg.gz or .sdimg)")
	bmapPath := flag.String("bmap", "", "Path to bmap file (optional, enables sparse writes)")
	devicePath := flag.String("device", "", "Target block device (e.g. /dev/sdb)")
	twoPhase := flag.Bool("two-phase", false, "Write partitions first, boot area last (safe flash)")
	bootBlocks := flag.Int("boot-blocks", 6, "Number of 4MB blocks in the boot area (for --two-phase)")
	showVersion := flag.Bool("version", false, "Show version")
	flag.Parse()

	if *showVersion {
		fmt.Println(version)
		os.Exit(0)
	}

	if *imagePath == "" || *devicePath == "" {
		fmt.Fprintf(os.Stderr, "Usage: librescoot-flasher --image IMAGE --device DEVICE [--bmap BMAP] [--two-phase]\n")
		os.Exit(1)
	}

	// On Windows, ensure disk is brought back online on exit
	defer cleanupPlatform()

	var err error
	if *bmapPath != "" {
		err = flashWithBmap(*imagePath, *bmapPath, *devicePath)
	} else if *twoPhase {
		err = flashTwoPhase(*imagePath, *devicePath, *bootBlocks)
	} else {
		err = flashSequential(*imagePath, *devicePath)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "DONE\n")
}

// progress reports bytes written to stderr, throttled to at most once per second.
var lastProgressTime time.Time

func progress(written int64) {
	now := time.Now()
	if now.Sub(lastProgressTime) >= time.Second {
		fmt.Fprintf(os.Stderr, "PROGRESS:%d\n", written)
		lastProgressTime = now
	}
}

func progressFinal(written int64) {
	fmt.Fprintf(os.Stderr, "PROGRESS:%d\n", written)
}

// openImage opens a firmware image, decompressing gzip if needed.
func openImage(path string) (io.ReadCloser, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			f.Close()
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return &gzipReadCloser{gz: gz, file: f}, nil
	}
	return f, nil
}

type gzipReadCloser struct {
	gz   *gzip.Reader
	file *os.File
}

func (g *gzipReadCloser) Read(p []byte) (int, error) { return g.gz.Read(p) }
func (g *gzipReadCloser) Close() error {
	g.gz.Close()
	return g.file.Close()
}

// openDevice opens a block device for writing.
func openDevice(path string) (*os.File, error) {
	return openDevicePlatform(path)
}

const blockSize = 4 * 1024 * 1024 // 4MB

// flashSequential writes the full image sequentially (no bmap).
func flashSequential(imagePath, devicePath string) error {
	fmt.Fprintf(os.Stderr, "Sequential flash: %s -> %s\n", imagePath, devicePath)

	// Report total size
	if fi, err := os.Stat(imagePath); err == nil {
		fmt.Fprintf(os.Stderr, "TOTAL:%d\n", fi.Size())
	}

	src, err := openImage(imagePath)
	if err != nil {
		return fmt.Errorf("opening image: %w", err)
	}
	defer src.Close()

	dev, err := openDevice(devicePath)
	if err != nil {
		return fmt.Errorf("opening device: %w", err)
	}
	defer dev.Close()

	buf := make([]byte, blockSize)
	var totalWritten int64
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err := dev.Write(buf[:n]); err != nil {
				return fmt.Errorf("write at offset %d: %w", totalWritten, err)
			}
			totalWritten += int64(n)
			progress(totalWritten)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return fmt.Errorf("read: %w", readErr)
		}
	}

	syncDevicePlatform(dev)
	progressFinal(totalWritten)
	fmt.Fprintf(os.Stderr, "Written %d bytes\n", totalWritten)
	return nil
}

// flashTwoPhase writes partitions first (skip boot area), then boot area last.
func flashTwoPhase(imagePath, devicePath string, bootBlocks int) error {
	bootBytes := int64(bootBlocks) * blockSize
	fmt.Fprintf(os.Stderr, "Two-phase flash: boot area = %d blocks (%d bytes)\n", bootBlocks, bootBytes)

	dev, err := openDevice(devicePath)
	if err != nil {
		return fmt.Errorf("opening device: %w", err)
	}
	defer dev.Close()

	// Phase A: write everything after boot area
	fmt.Fprintf(os.Stderr, "PHASE:A\n")
	src, err := openImage(imagePath)
	if err != nil {
		return err
	}
	// Skip boot area in source
	if _, err := io.CopyN(io.Discard, src, bootBytes); err != nil {
		src.Close()
		return fmt.Errorf("skipping boot area in source: %w", err)
	}
	// Seek device past boot area
	if _, err := dev.Seek(bootBytes, io.SeekStart); err != nil {
		src.Close()
		return fmt.Errorf("seeking device: %w", err)
	}

	buf := make([]byte, blockSize)
	var written int64
	for {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err := dev.Write(buf[:n]); err != nil {
				src.Close()
				return fmt.Errorf("phase A write: %w", err)
			}
			written += int64(n)
			progress(written)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			src.Close()
			return fmt.Errorf("phase A read: %w", readErr)
		}
	}
	src.Close()
	syncDevicePlatform(dev)
	progressFinal(written)
	fmt.Fprintf(os.Stderr, "Phase A: %d bytes written\n", written)

	// Phase B: write boot area
	fmt.Fprintf(os.Stderr, "PHASE:B\n")
	src, err = openImage(imagePath)
	if err != nil {
		return err
	}
	if _, err := dev.Seek(0, io.SeekStart); err != nil {
		src.Close()
		return fmt.Errorf("seeking device to start: %w", err)
	}

	written = 0
	for written < bootBytes {
		n, readErr := io.ReadFull(src, buf)
		if n > 0 {
			if _, err := dev.Write(buf[:n]); err != nil {
				src.Close()
				return fmt.Errorf("phase B write: %w", err)
			}
			written += int64(n)
			progress(written)
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			src.Close()
			return fmt.Errorf("phase B read: %w", readErr)
		}
	}
	src.Close()
	syncDevicePlatform(dev)
	progressFinal(written)
	fmt.Fprintf(os.Stderr, "Phase B: %d bytes written (boot area)\n", written)

	return nil
}

// Bmap XML structures
type bmapXML struct {
	XMLName           xml.Name     `xml:"bmap"`
	Version           string       `xml:"version,attr"`
	ImageSize         int64        `xml:"ImageSize"`
	BlockSize         int64        `xml:"BlockSize"`
	BlocksCount       int64        `xml:"BlocksCount"`
	MappedBlocksCount int64        `xml:"MappedBlocksCount"`
	ChecksumType      string       `xml:"ChecksumType"`
	BlockMap          bmapBlockMap `xml:"BlockMap"`
}

type bmapBlockMap struct {
	Ranges []bmapRange `xml:"Range"`
}

type bmapRange struct {
	Chksum string `xml:"chksum,attr"`
	Value  string `xml:",chardata"`
}

func (r bmapRange) parse() (start, end int64, err error) {
	parts := strings.SplitN(strings.TrimSpace(r.Value), "-", 2)
	start, err = strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, 0, err
	}
	if len(parts) == 2 {
		end, err = strconv.ParseInt(parts[1], 10, 64)
	} else {
		end = start
	}
	return
}

// bootAreaBytes is the size of the U-Boot env / bootloader region kept
// intact during phase A so a mid-flash failure leaves the device able to
// re-enter UMS mode on next boot. Matches BOOT_AREA_BLOCKS=6 (* 4 MB)
// used by the trampoline and the installer's two-phase fallback.
const bootAreaBytes int64 = 24 * 1024 * 1024

// flashWithBmap writes only mapped blocks from the bmap, in two passes:
//
//  Phase A: write everything outside the first 24 MB. Per-range SHA-256
//           checksums (bmap-published) are verified during this pass —
//           we read each range fully even when only part of it gets
//           written, so the hash covers the same bytes the bmap signed.
//  Sync.
//  Phase B: re-open the source and write the deferred boot-area bytes.
//
// If anything fails before Phase B completes the U-Boot env is still the
// pre-flash one, so the device boots back into UMS and the user can
// retry without re-running the prep step.
func flashWithBmap(imagePath, bmapPath, devicePath string) error {
	bmapData, err := os.ReadFile(bmapPath)
	if err != nil {
		return fmt.Errorf("reading bmap: %w", err)
	}
	var bmap bmapXML
	if err := xml.Unmarshal(bmapData, &bmap); err != nil {
		return fmt.Errorf("parsing bmap: %w", err)
	}

	mappedBytes := bmap.MappedBlocksCount * bmap.BlockSize
	fmt.Fprintf(os.Stderr, "TOTAL:%d\n", mappedBytes)
	fmt.Fprintf(os.Stderr, "Bmap: %d/%d blocks mapped (%d%% of %d bytes), block size %d\n",
		bmap.MappedBlocksCount, bmap.BlocksCount,
		bmap.MappedBlocksCount*100/bmap.BlocksCount,
		bmap.ImageSize, bmap.BlockSize)

	bs := bmap.BlockSize
	if bs == 0 {
		bs = 4096
	}

	dev, err := openDevice(devicePath)
	if err != nil {
		return fmt.Errorf("opening device: %w", err)
	}
	defer dev.Close()

	// Phase A: non-boot area, hash every range.
	fmt.Fprintf(os.Stderr, "PHASE:A\n")
	srcA, err := openImage(imagePath)
	if err != nil {
		return fmt.Errorf("opening image (phase A): %w", err)
	}
	writtenA, checksumErrors, err := bmapPass(srcA, dev, bmap, bs, bootAreaBytes, false, 0)
	srcA.Close()
	if err != nil {
		return err
	}
	if err := syncDevicePlatform(dev); err != nil {
		return fmt.Errorf("sync after phase A: %w", err)
	}
	fmt.Fprintf(os.Stderr, "Phase A: %d bytes written\n", writtenA)

	// Phase B: boot area only.
	fmt.Fprintf(os.Stderr, "PHASE:B\n")
	srcB, err := openImage(imagePath)
	if err != nil {
		return fmt.Errorf("opening image (phase B): %w", err)
	}
	defer srcB.Close()
	writtenB, _, err := bmapPass(srcB, dev, bmap, bs, bootAreaBytes, true, writtenA)
	if err != nil {
		return err
	}
	if err := syncDevicePlatform(dev); err != nil {
		return fmt.Errorf("sync after phase B: %w", err)
	}

	totalWritten := writtenA + writtenB
	progressFinal(totalWritten)
	fmt.Fprintf(os.Stderr, "Phase B: %d bytes written (boot area)\n", writtenB)
	fmt.Fprintf(os.Stderr, "Written %d bytes (%d mapped blocks)\n", totalWritten, bmap.MappedBlocksCount)

	if checksumErrors > 0 {
		return fmt.Errorf("%d checksum errors detected", checksumErrors)
	}
	return nil
}

// bmapPass walks the bmap ranges once. When bootPass is false (Phase A)
// it reads & hashes every range and writes only the bytes >= bootBytes,
// verifying per-range SHA-256 against the bmap. When bootPass is true
// (Phase B) it skips the source for ranges entirely outside the boot
// area and writes only the bytes < bootBytes.
//
// startProgress lets Phase B's progress counter pick up where Phase A
// left off so the host UI sees a monotonic total.
func bmapPass(src io.Reader, dev io.WriteSeeker, bmap bmapXML, bs int64, bootBytes int64, bootPass bool, startProgress int64) (int64, int, error) {
	const writeBufSize = 4 * 1024 * 1024
	buf := make([]byte, writeBufSize)

	var srcPos int64
	var phaseWritten int64
	var checksumErrors int

	cumulative := startProgress

	for _, rng := range bmap.BlockMap.Ranges {
		start, end, err := rng.parse()
		if err != nil {
			return phaseWritten, checksumErrors, fmt.Errorf("parsing range %q: %w", rng.Value, err)
		}
		rangeStart := start * bs
		rangeEnd := (end + 1) * bs

		// Phase B can short-circuit ranges that don't touch the boot area.
		if bootPass && rangeStart >= bootBytes {
			if rangeEnd > srcPos {
				skip := rangeEnd - srcPos
				if _, err := io.CopyN(io.Discard, src, skip); err != nil {
					return phaseWritten, checksumErrors, fmt.Errorf("phase B skip to %d: %w", rangeEnd, err)
				}
				srcPos = rangeEnd
			}
			continue
		}

		// Discard unmapped source bytes before this range starts.
		if rangeStart > srcPos {
			skip := rangeStart - srcPos
			if _, err := io.CopyN(io.Discard, src, skip); err != nil {
				return phaseWritten, checksumErrors, fmt.Errorf("skip to offset %d: %w", rangeStart, err)
			}
			srcPos = rangeStart
		}

		// Compute the [writeStart, writeEnd) sub-range we want to commit
		// in this pass. Phase A: anything >= bootBytes. Phase B: anything
		// < bootBytes.
		var writeStart, writeEnd int64
		if bootPass {
			writeStart = rangeStart
			writeEnd = rangeEnd
			if writeEnd > bootBytes {
				writeEnd = bootBytes
			}
		} else {
			writeStart = rangeStart
			if writeStart < bootBytes {
				writeStart = bootBytes
			}
			writeEnd = rangeEnd
		}

		var h hash.Hash
		if !bootPass {
			h = sha256.New()
		}

		if writeStart < writeEnd {
			if _, err := dev.Seek(writeStart, io.SeekStart); err != nil {
				return phaseWritten, checksumErrors, fmt.Errorf("seek to %d: %w", writeStart, err)
			}
		}

		// Read the entire range from source so the hash covers exactly
		// what the bmap signed. Write only the chunk that overlaps
		// [writeStart, writeEnd).
		rangeRemaining := rangeEnd - rangeStart
		rangeOffset := rangeStart
		for rangeRemaining > 0 {
			readSize := int64(len(buf))
			if readSize > rangeRemaining {
				readSize = rangeRemaining
			}
			n, readErr := io.ReadFull(src, buf[:readSize])
			if n > 0 {
				if h != nil {
					h.Write(buf[:n])
				}
				if writeStart < writeEnd {
					chunkStart := rangeOffset
					chunkEnd := chunkStart + int64(n)
					inS := writeStart
					if chunkStart > inS {
						inS = chunkStart
					}
					inE := writeEnd
					if chunkEnd < inE {
						inE = chunkEnd
					}
					if inS < inE {
						off := inS - chunkStart
						length := inE - inS
						if _, err := dev.Write(buf[off : off+length]); err != nil {
							return phaseWritten, checksumErrors, fmt.Errorf("write at offset %d: %w", inS, err)
						}
						phaseWritten += length
						cumulative += length
						progress(cumulative)
					}
				}
				srcPos += int64(n)
				rangeOffset += int64(n)
				rangeRemaining -= int64(n)
			}
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				break
			}
			if readErr != nil {
				return phaseWritten, checksumErrors, fmt.Errorf("read at offset %d: %w", rangeOffset, readErr)
			}
		}

		// Per-range checksum is only meaningful in Phase A where we
		// hashed the full source bytes for the range.
		if !bootPass && rng.Chksum != "" && bmap.ChecksumType == "sha256" {
			got := hex.EncodeToString(h.Sum(nil))
			if got != rng.Chksum {
				fmt.Fprintf(os.Stderr, "CHECKSUM MISMATCH range %d-%d: expected %s, got %s\n",
					start, end, rng.Chksum, got)
				checksumErrors++
			}
		}
	}

	return phaseWritten, checksumErrors, nil
}
