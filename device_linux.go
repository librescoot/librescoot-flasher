//go:build linux

package main

import (
	"os"
	"syscall"
)

func openDevicePlatform(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|syscall.O_DIRECT, 0)
}

func cleanupPlatform() {}

func syncDevicePlatform(f *os.File) error { return f.Sync() }

