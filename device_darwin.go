//go:build darwin

package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

func openDevicePlatform(path string) (*os.File, error) {
	// If we're already root (e.g. installer self-elevated via osascript), skip
	// authopen and open the device directly. authopen invoked from a root
	// parent in a non-GUI session hits EPERM on /dev/rdiskN even though a
	// plain root open(2) works fine.
	if os.Geteuid() == 0 {
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return nil, err
		}
		// F_NOCACHE for direct I/O (macOS equivalent of O_DIRECT)
		syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_NOCACHE, 1)
		return f, nil
	}

	// Create unix socket pair for fd passing
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		return nil, fmt.Errorf("socketpair: %w", err)
	}
	parentSock := os.NewFile(uintptr(fds[0]), "parent-sock")
	childSock := os.NewFile(uintptr(fds[1]), "child-sock")

	// Launch authopen — it handles its own authorization dialog (single prompt)
	// and sends the authorized fd back via SCM_RIGHTS on stdout.
	modeStr := strconv.Itoa(syscall.O_RDWR)
	cmd := exec.Command("/usr/libexec/authopen",
		"-stdoutpipe",
		"-o", modeStr,
		path,
	)
	cmd.Stdout = childSock
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		parentSock.Close()
		childSock.Close()
		return nil, fmt.Errorf("starting authopen: %w", err)
	}
	childSock.Close()

	// Receive the device fd via SCM_RIGHTS
	deviceFd, err := receiveFd(int(parentSock.Fd()))
	parentSock.Close()

	waitErr := cmd.Wait()
	if err != nil {
		return nil, fmt.Errorf("receiving device fd: %w", err)
	}
	if waitErr != nil {
		if deviceFd >= 0 {
			syscall.Close(deviceFd)
		}
		return nil, fmt.Errorf("authopen failed: %w", waitErr)
	}

	// F_NOCACHE for direct I/O (macOS equivalent of O_DIRECT)
	syscall.Syscall(syscall.SYS_FCNTL, uintptr(deviceFd), syscall.F_NOCACHE, 1)

	return os.NewFile(uintptr(deviceFd), path), nil
}

func cleanupPlatform() {}

// syncDevicePlatform forces buffered writes to durable storage. On macOS,
// fsync(2) on /dev/rdiskN frequently returns ENOTTY because raw character
// devices don't implement it; fcntl(F_FULLFSYNC) is the documented call
// that flushes the device's own write cache and is what dd, asr and Time
// Machine use. We try F_FULLFSYNC first and fall back to plain Sync().
// ENOTTY/EINVAL/ENODEV from either is treated as a no-op since we already
// opened with F_NOCACHE (writes bypass the OS buffer cache).
func finishDevicePlatform(f *os.File) error { return nil }

func syncDevicePlatform(f *os.File) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_FULLFSYNC, 0); errno == 0 {
		return nil
	} else if errno != syscall.ENOTTY && errno != syscall.EINVAL && errno != syscall.ENODEV {
		return fmt.Errorf("fcntl F_FULLFSYNC: %w", errno)
	}
	if err := f.Sync(); err != nil {
		if pe, ok := err.(*os.PathError); ok {
			if pe.Err == syscall.ENOTTY || pe.Err == syscall.EINVAL || pe.Err == syscall.ENODEV {
				return nil
			}
		}
		return err
	}
	return nil
}

func receiveFd(sock int) (int, error) {
	f := os.NewFile(uintptr(sock), "sock")
	defer f.Close()

	conn, err := net.FileConn(f)
	if err != nil {
		return -1, err
	}
	defer conn.Close()

	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return -1, fmt.Errorf("not a unix connection")
	}

	buf := make([]byte, 1)
	oob := make([]byte, syscall.CmsgLen(4))
	_, oobn, _, _, err := uc.ReadMsgUnix(buf, oob)
	if err != nil {
		return -1, fmt.Errorf("recvmsg: %w", err)
	}

	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return -1, fmt.Errorf("parsing control message: %w", err)
	}

	for _, msg := range msgs {
		fds, err := syscall.ParseUnixRights(&msg)
		if err != nil {
			continue
		}
		if len(fds) > 0 {
			return fds[0], nil
		}
	}

	return -1, fmt.Errorf("no fd received from authopen")
}
