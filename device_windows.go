//go:build windows

package main

import (
	"os"
	"syscall"
)

func openDevicePlatform(path string) (*os.File, error) {
	// Windows: FILE_FLAG_NO_BUFFERING = 0x20000000
	const FILE_FLAG_NO_BUFFERING = 0x20000000
	return os.OpenFile(path, os.O_RDWR|syscall.O_SYNC, 0)
	// TODO: use CreateFile with FILE_FLAG_NO_BUFFERING for true direct I/O
}
