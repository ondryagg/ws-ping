//go:build linux

// Package sysinfo reads process-level resource figures.
package sysinfo

import (
	"os"
	"syscall"
)

// OpenFDs counts this process's open file descriptors.
func OpenFDs() (int, error) {
	f, err := os.Open("/proc/self/fd")
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	if err != nil {
		return 0, err
	}
	return len(names) - 1, nil // minus the descriptor used for this listing
}

// FDLimit is the soft RLIMIT_NOFILE.
func FDLimit() (uint64, error) {
	var r syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &r); err != nil {
		return 0, err
	}
	return uint64(r.Cur), nil //nolint:unconvert // Rlimit fields differ by platform
}
