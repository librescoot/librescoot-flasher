package main

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
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

	dev.Sync()
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
	dev.Sync()
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
	dev.Sync()
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

// flashWithBmap writes only mapped blocks from the bmap.
func flashWithBmap(imagePath, bmapPath, devicePath string) error {
	// Parse bmap
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

	bs := bmap.BlockSize
	if bs == 0 {
		bs = 4096
	}
	// Use 4MB buffer for bulk writes (bmap block size is typically 4KB)
	const writeBufSize = 4 * 1024 * 1024
	buf := make([]byte, writeBufSize)

	var srcPos int64
	var totalWritten int64
	var checksumErrors int

	for _, rng := range bmap.BlockMap.Ranges {
		start, end, err := rng.parse()
		if err != nil {
			return fmt.Errorf("parsing range %q: %w", rng.Value, err)
		}

		rangeStart := start * bs
		rangeEnd := (end + 1) * bs

		// Skip unneeded source data (unmapped blocks before this range)
		if rangeStart > srcPos {
			skip := rangeStart - srcPos
			if _, err := io.CopyN(io.Discard, src, skip); err != nil {
				return fmt.Errorf("skipping to offset %d: %w", rangeStart, err)
			}
			srcPos = rangeStart
		}

		// Seek device to range start
		if _, err := dev.Seek(rangeStart, io.SeekStart); err != nil {
			return fmt.Errorf("seeking device to %d: %w", rangeStart, err)
		}

		// Write this range in large chunks, computing checksum
		h := sha256.New()
		rangeRemaining := rangeEnd - rangeStart
		for rangeRemaining > 0 {
			readSize := int64(len(buf))
			if readSize > rangeRemaining {
				readSize = rangeRemaining
			}
			n, readErr := io.ReadFull(src, buf[:readSize])
			if n > 0 {
				h.Write(buf[:n])
				if _, err := dev.Write(buf[:n]); err != nil {
					return fmt.Errorf("write at offset %d: %w", rangeEnd-rangeRemaining, err)
				}
				totalWritten += int64(n)
				srcPos += int64(n)
				rangeRemaining -= int64(n)
				progress(totalWritten)
			}
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				break
			}
			if readErr != nil {
				return fmt.Errorf("read at offset %d: %w", rangeEnd-rangeRemaining, readErr)
			}
		}

		// Verify checksum
		if rng.Chksum != "" && bmap.ChecksumType == "sha256" {
			got := hex.EncodeToString(h.Sum(nil))
			if got != rng.Chksum {
				fmt.Fprintf(os.Stderr, "CHECKSUM MISMATCH range %d-%d: expected %s, got %s\n",
					start, end, rng.Chksum, got)
				checksumErrors++
			}
		}

	}

	dev.Sync()
	progressFinal(totalWritten)
	fmt.Fprintf(os.Stderr, "Written %d bytes (%d mapped blocks)\n", totalWritten, bmap.MappedBlocksCount)

	if checksumErrors > 0 {
		return fmt.Errorf("%d checksum errors detected", checksumErrors)
	}
	return nil
}
