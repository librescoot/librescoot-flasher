//go:build windows

package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procCreateFileW     = kernel32.NewProc("CreateFileW")
	procDeviceIoControl = kernel32.NewProc("DeviceIoControl")
)

const (
	GENERIC_READ                 = 0x80000000
	GENERIC_WRITE                = 0x40000000
	FILE_SHARE_READ              = 0x1
	FILE_SHARE_WRITE             = 0x2
	OPEN_EXISTING                = 3
	FILE_FLAG_WRITE_THROUGH      = 0x80000000
	FSCTL_LOCK_VOLUME            = 0x00090018
	FSCTL_DISMOUNT_VOLUME        = 0x00090020
	FSCTL_ALLOW_EXTENDED_DASD_IO = 0x00090083
)

// diskNumber extracted during open, used for cleanup
var openedDiskNumber string

func openDevicePlatform(path string) (*os.File, error) {
	// Extract disk number for PowerShell commands
	upper := strings.ToUpper(path)
	if idx := strings.Index(upper, "PHYSICALDRIVE"); idx >= 0 {
		openedDiskNumber = path[idx+len("PHYSICALDRIVE"):]
	}

	if openedDiskNumber != "" {
		// Take disk offline — this removes ALL volumes from the Windows
		// storage stack, preventing any filesystem driver from blocking
		// our raw writes. This is the only reliable way to get exclusive
		// access to a disk with partitions that Windows recognizes.
		fmt.Fprintf(os.Stderr, "LOCK: taking disk %s offline\n", openedDiskNumber)
		offlineScript := fmt.Sprintf(
			`Set-Disk -Number %s -IsOffline $true -ErrorAction Stop`,
			openedDiskNumber,
		)
		cmd := exec.Command("powershell", "-NoProfile", "-Command", offlineScript)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stderr, "LOCK: offline failed: %v (%s), trying to continue\n", err, strings.TrimSpace(string(out)))
		} else {
			fmt.Fprintf(os.Stderr, "LOCK: disk offline OK\n")
		}
	}

	// Open the physical drive
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
	if h == uintptr(syscall.InvalidHandle) {
		return nil, fmt.Errorf("CreateFile %s: %w", path, errno)
	}

	handle := syscall.Handle(h)
	var bytesReturned uint32

	// Lock the volume for exclusive access
	r1, _, _ := procDeviceIoControl.Call(
		uintptr(handle), FSCTL_LOCK_VOLUME,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)
	if r1 != 0 {
		fmt.Fprintf(os.Stderr, "LOCK: volume locked\n")
	} else {
		// Dismount and retry
		procDeviceIoControl.Call(
			uintptr(handle), FSCTL_DISMOUNT_VOLUME,
			0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
		)
		r2, _, _ := procDeviceIoControl.Call(
			uintptr(handle), FSCTL_LOCK_VOLUME,
			0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
		)
		if r2 != 0 {
			fmt.Fprintf(os.Stderr, "LOCK: locked after dismount\n")
		}
	}

	// Dismount any remaining filesystem mounts
	procDeviceIoControl.Call(
		uintptr(handle), FSCTL_DISMOUNT_VOLUME,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)

	// Allow writes past reported disk end
	procDeviceIoControl.Call(
		uintptr(handle), FSCTL_ALLOW_EXTENDED_DASD_IO,
		0, 0, 0, 0, uintptr(unsafe.Pointer(&bytesReturned)), 0,
	)

	fmt.Fprintf(os.Stderr, "LOCK: disk ready for writing\n")
	return os.NewFile(uintptr(handle), path), nil
}

func cleanupPlatform() {
	BringDiskOnline()
}

// BringDiskOnline should be called after flashing to restore the disk.
func BringDiskOnline() {
	if openedDiskNumber == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "LOCK: bringing disk %s back online\n", openedDiskNumber)
	cmd := exec.Command("powershell", "-NoProfile", "-Command",
		fmt.Sprintf(`Set-Disk -Number %s -IsOffline $false -ErrorAction SilentlyContinue; Set-Disk -Number %s -IsReadOnly $false -ErrorAction SilentlyContinue`, openedDiskNumber, openedDiskNumber),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "LOCK: online failed: %v (%s)\n", err, strings.TrimSpace(string(out)))
	} else {
		fmt.Fprintf(os.Stderr, "LOCK: disk online OK\n")
	}
}
