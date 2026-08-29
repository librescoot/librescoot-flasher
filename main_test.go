package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func TestVerifyBmapReadbackChecksCompleteBootRanges(t *testing.T) {
	const bs = int64(4096)
	device := make([]byte, 4*bs)
	for i := range device {
		device[i] = byte(i % 251)
	}
	bmap := bmapXML{
		ChecksumType: " sha256 ",
		BlockMap: bmapBlockMap{Ranges: []bmapRange{
			{Value: "0-1", Chksum: checksum(device[0 : 2*bs])},
			{Value: "3", Chksum: checksum(device[3*bs : 4*bs])},
		}},
	}

	verified, mismatches, err := verifyBmapReadback(
		bytes.NewReader(device), bmap, bs, int64(len(device)),
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := int64(3 * bs); verified != want {
		t.Fatalf("verified %d bytes, want %d", verified, want)
	}
	if len(mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", mismatches)
	}
}

func TestVerifyBmapReadbackReportsDeviceMismatch(t *testing.T) {
	const bs = int64(4096)
	device := bytes.Repeat([]byte{0x5a}, int(bs))
	bmap := bmapXML{
		ChecksumType: "sha256",
		BlockMap: bmapBlockMap{Ranges: []bmapRange{
			{Value: "0", Chksum: checksum(bytes.Repeat([]byte{0xa5}, int(bs)))},
		}},
	}

	_, mismatches, err := verifyBmapReadback(
		bytes.NewReader(device), bmap, bs, bs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(mismatches) != 1 || mismatches[0] != "0" {
		t.Fatalf("mismatches = %v, want [0]", mismatches)
	}
	if got := formatRangeFailures(mismatches, 5); got != " (device ranges: 0)" {
		t.Fatalf("formatted mismatch = %q", got)
	}
}

func TestVerifyBmapReadbackChecksSmallBoundaryCrossingRange(t *testing.T) {
	const bs = int64(4096)
	device := bytes.Repeat([]byte{0x33}, int(4*bs))
	bmap := bmapXML{
		ChecksumType: "sha256",
		BlockMap: bmapBlockMap{Ranges: []bmapRange{
			{Value: "0", Chksum: checksum(device[:bs])},
			{Value: "1-3", Chksum: checksum(device[bs:])},
		}},
	}

	verified, mismatches, err := verifyBmapReadback(
		bytes.NewReader(device), bmap, bs, 2*bs,
	)
	if err != nil {
		t.Fatal(err)
	}
	if verified != 4*bs {
		t.Fatalf("verified %d bytes, want %d", verified, 4*bs)
	}
	if len(mismatches) != 0 {
		t.Fatalf("unexpected mismatches: %v", mismatches)
	}
}
