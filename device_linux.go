//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	blkroset = 0x125d
	blkroget = 0x125e
)

// openDevicePlatform claims the whole block device exclusively. Linux applies
// this claim to its partitions as well: the open fails with EBUSY if any child
// is mounted, and new mounts fail for as long as this descriptor remains open.
// This closes the desktop-automounter race between the installer's unmount and
// the raw write.
func openDevicePlatform(path string) (*os.File, error) {
	flags := os.O_RDWR | syscall.O_DIRECT | syscall.O_EXCL
	for attempt := 1; attempt <= 3; attempt++ {
		f, err := os.OpenFile(path, flags, 0)
		if err == nil {
			var readOnly int32
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkroget, uintptr(unsafe.Pointer(&readOnly)))
			if errno != 0 {
				f.Close()
				return nil, fmt.Errorf("reading device read-only flag: %w", errno)
			}
			if readOnly != 0 {
				value := int32(0)
				_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkroset, uintptr(unsafe.Pointer(&value)))
				if errno != 0 {
					f.Close()
					return nil, fmt.Errorf("making claimed device writable: %w", errno)
				}
				fmt.Fprintf(os.Stderr, "CLAIM: cleared host read-only flag\n")
			}
			fmt.Fprintf(os.Stderr, "CLAIM: exclusive block-device claim acquired\n")
			return f, nil
		}

		// A previous diagnostic or interrupted run may deliberately leave the
		// host-side block device read-only. Change only that kernel flag, while
		// holding an exclusive read descriptor, then retry the writable claim.
		if errors.Is(err, syscall.EROFS) {
			ro, roErr := os.OpenFile(path, os.O_RDONLY|syscall.O_EXCL, 0)
			if roErr != nil {
				return nil, fmt.Errorf("claiming read-only device: %w", roErr)
			}
			value := int32(0)
			_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, ro.Fd(), blkroset, uintptr(unsafe.Pointer(&value)))
			ro.Close()
			if errno != 0 {
				return nil, fmt.Errorf("making device writable: %w", errno)
			}
			fmt.Fprintf(os.Stderr, "CLAIM: cleared host read-only flag\n")
			continue
		}

		if errors.Is(err, syscall.EBUSY) && attempt < 3 {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		return nil, fmt.Errorf("exclusive block-device claim: %w", err)
	}
	return nil, fmt.Errorf("exclusive block-device claim remained busy")
}

func cleanupPlatform() {}

// O_DIRECT requires the userspace buffer itself to be page-aligned. mmap
// provides that guarantee and lets the same descriptor be used for the
// post-sync readback without releasing the exclusive block-device claim.
func allocDeviceBuffer(size int) ([]byte, func(), error) {
	buf, err := syscall.Mmap(
		-1, 0, size,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_PRIVATE|syscall.MAP_ANON,
	)
	if err != nil {
		return nil, nil, err
	}
	return buf, func() { _ = syscall.Munmap(buf) }, nil
}

func syncDevicePlatform(f *os.File) error { return f.Sync() }

// Leave the exported disk read-only before dropping the exclusive claim. That
// makes the small post-flash/pre-reboot window harmless even if a desktop
// automounter immediately notices the partitions again.
func finishDevicePlatform(f *os.File) error {
	value := int32(1)
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), blkroset, uintptr(unsafe.Pointer(&value)))
	if errno != 0 {
		return errno
	}
	fmt.Fprintf(os.Stderr, "CLAIM: device marked read-only before release\n")
	return nil
}
