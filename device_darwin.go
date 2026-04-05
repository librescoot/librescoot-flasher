//go:build darwin

package main

import (
	"os"
	"syscall"
)

func openDevicePlatform(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	// macOS equivalent of O_DIRECT: F_NOCACHE
	syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_NOCACHE, 1)
	return f, nil
}
