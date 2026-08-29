//go:build windows

package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	kernel32             = syscall.NewLazyDLL("kernel32.dll")
	procCreateFileW      = kernel32.NewProc("CreateFileW")
	procDeviceIoControl  = kernel32.NewProc("DeviceIoControl")
	procFindFirstVolumeW = kernel32.NewProc("FindFirstVolumeW")
	procFindNextVolumeW  = kernel32.NewProc("FindNextVolumeW")
	procFindVolumeClose  = kernel32.NewProc("FindVolumeClose")
)

const (
	GENERIC_READ                    = 0x80000000
	GENERIC_WRITE                   = 0x40000000
	FILE_SHARE_READ                 = 0x1
	FILE_SHARE_WRITE                = 0x2
	OPEN_EXISTING                   = 3
	FILE_FLAG_WRITE_THROUGH         = 0x80000000
	FSCTL_LOCK_VOLUME               = 0x00090018
	FSCTL_DISMOUNT_VOLUME           = 0x00090020
	FSCTL_ALLOW_EXTENDED_DASD_IO    = 0x00090083
	IOCTL_STORAGE_GET_DEVICE_NUMBER = 0x002D1080
)

// invalidHandle mirrors INVALID_HANDLE_VALUE for both 32- and 64-bit builds.
var invalidHandle = ^uintptr(0)

// storageDeviceNumber matches Win32 STORAGE_DEVICE_NUMBER.
type storageDeviceNumber struct {
	DeviceType      uint32
	DeviceNumber    uint32
	PartitionNumber uint32
}

// Volume handles we hold for the lifetime of the flash. Keeping these open
// prevents Windows from remounting the partitions and reclaiming write
// ranges mid-flash. Released by cleanupPlatform on exit.
var heldVolumeHandles []syscall.Handle

func openDevicePlatform(path string) (*os.File, error) {
	upper := strings.ToUpper(path)
	idx := strings.Index(upper, "PHYSICALDRIVE")
	if idx < 0 {
		return nil, fmt.Errorf("expected PhysicalDrive path, got %s", path)
	}
	diskNum, err := strconv.ParseUint(path[idx+len("PHYSICALDRIVE"):], 10, 32)
	if err != nil {
		return nil, fmt.Errorf("invalid disk number in %q: %w", path, err)
	}

	// Lock and dismount every volume on this disk before opening the raw
	// drive. This is the Rufus / Win32 Disk Imager / Etcher approach: a
	// raw write to a disk Windows recognizes as having mounted partitions
	// will be blocked by the filesystem driver ("Access is denied") the
	// moment writes cross into a volume's range. Locking each volume
	// handle and keeping it open for the duration of the flash is the
	// only reliable way around this — Set-Disk -IsOffline does not work
	// for many USB-attached gadgets (incl. devices in u-boot UMS mode).
	lockVolumesOnDisk(uint32(diskNum))

	pathW, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, _, errno := procCreateFileW.Call(
		uintptr(unsafe.Pointer(pathW)),
		GENERIC_READ|GENERIC_WRITE,
		FILE_SHARE_READ|FILE_SHARE_WRITE,
		0,
		OPEN_EXISTING,
		FILE_FLAG_WRITE_THROUGH,
		0,
	)
	if h == invalidHandle {
		return nil, fmt.Errorf("CreateFile %s: %w", path, errno)
	}
	handle := syscall.Handle(h)

	var bytesReturned uint32
	// Belt-and-suspenders: also lock + dismount the physical-drive handle.
	procDeviceIoControl.Call(
		uintptr(handle), FSCTL_LOCK_VOLUME,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)
	procDeviceIoControl.Call(
		uintptr(handle), FSCTL_DISMOUNT_VOLUME,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)
	procDeviceIoControl.Call(
		uintptr(handle), FSCTL_ALLOW_EXTENDED_DASD_IO,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)

	fmt.Fprintf(os.Stderr, "LOCK: physical drive %d ready for writing\n", diskNum)
	return os.NewFile(uintptr(handle), path), nil
}

// lockVolumesOnDisk enumerates every volume the OS knows about, filters to
// the ones that live on the given physical disk number, and locks +
// dismounts each one. Best-effort: failures are logged but do not abort.
func lockVolumesOnDisk(diskNum uint32) {
	const bufLen = 64
	buf := make([]uint16, bufLen)
	hFind, _, errno := procFindFirstVolumeW.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		bufLen,
	)
	if hFind == invalidHandle {
		fmt.Fprintf(os.Stderr, "LOCK: FindFirstVolume failed: %v\n", errno)
		return
	}
	defer procFindVolumeClose.Call(hFind)

	for {
		volPath := syscall.UTF16ToString(buf)
		// Volume paths come back as `\\?\Volume{GUID}\` — strip the
		// trailing backslash to get a path you can pass to CreateFile.
		opener := strings.TrimRight(volPath, "\\")
		tryLockVolume(opener, diskNum)

		for i := range buf {
			buf[i] = 0
		}
		ret, _, _ := procFindNextVolumeW.Call(
			hFind,
			uintptr(unsafe.Pointer(&buf[0])),
			bufLen,
		)
		if ret == 0 {
			break // ERROR_NO_MORE_FILES
		}
	}
}

// tryLockVolume opens the volume, checks whether it lives on the target
// disk, and if so locks + dismounts it and stashes the handle in
// heldVolumeHandles. The handle is intentionally leaked across the write so
// Windows cannot remount the volume in the middle of the flash.
func tryLockVolume(devicePath string, targetDisk uint32) {
	pathW, err := syscall.UTF16PtrFromString(devicePath)
	if err != nil {
		return
	}
	h, _, errno := procCreateFileW.Call(
		uintptr(unsafe.Pointer(pathW)),
		GENERIC_READ|GENERIC_WRITE,
		FILE_SHARE_READ|FILE_SHARE_WRITE,
		0,
		OPEN_EXISTING,
		0,
		0,
	)
	if h == invalidHandle {
		// Not all volumes are openable (e.g. some system / hidden ones);
		// don't make noise for those.
		_ = errno
		return
	}
	handle := syscall.Handle(h)

	var sdn storageDeviceNumber
	var bytesReturned uint32
	r, _, _ := procDeviceIoControl.Call(
		uintptr(handle),
		IOCTL_STORAGE_GET_DEVICE_NUMBER,
		0, 0,
		uintptr(unsafe.Pointer(&sdn)),
		unsafe.Sizeof(sdn),
		uintptr(unsafe.Pointer(&bytesReturned)),
		0,
	)
	if r == 0 || sdn.DeviceNumber != targetDisk {
		syscall.CloseHandle(handle)
		return
	}

	// Windows can transiently fail FSCTL_LOCK_VOLUME if a service has just
	// touched the volume (SearchIndexer, ShellHWDetection, antivirus).
	// Retry a handful of times like Rufus does.
	const maxAttempts = 10
	locked := false
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		r, _, _ := procDeviceIoControl.Call(
			uintptr(handle), FSCTL_LOCK_VOLUME,
			0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
		)
		if r != 0 {
			locked = true
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if !locked {
		fmt.Fprintf(os.Stderr, "LOCK: %s: lock failed after %d attempts\n", devicePath, maxAttempts)
		// Continue anyway: the dismount below may still free enough state.
	}

	procDeviceIoControl.Call(
		uintptr(handle), FSCTL_DISMOUNT_VOLUME,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)

	heldVolumeHandles = append(heldVolumeHandles, handle)
	fmt.Fprintf(os.Stderr, "LOCK: held volume %s (disk %d, partition %d)\n",
		devicePath, sdn.DeviceNumber, sdn.PartitionNumber)
}

func cleanupPlatform() {
	// Release in reverse order so the OS rescans cleanly.
	for i := len(heldVolumeHandles) - 1; i >= 0; i-- {
		syscall.CloseHandle(heldVolumeHandles[i])
	}
	heldVolumeHandles = nil
}

func syncDevicePlatform(f *os.File) error { return f.Sync() }

func finishDevicePlatform(f *os.File) error { return nil }
