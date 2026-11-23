//go:build linux && !android

package main

import (
	"errors"
	"os"
	"syscall"
)

const fallocFlKeepSize = 0x01

// preallocateFile reserves contiguous disk blocks on Linux using fallocate with FALLOC_FL_KEEP_SIZE.
// Unsupported filesystems (e.g. FAT/exFAT/tmpfs/network mounts) fall back gracefully without error.
func preallocateFile(f *os.File, size int64) error {
	if f == nil || size <= 0 {
		return nil
	}
	err := syscall.Fallocate(int(f.Fd()), fallocFlKeepSize, 0, size)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return err
	}
	// Ignore unsupported filesystem errors (EOPNOTSUPP, ENOTSUP, ENOSYS, EINVAL, EPERM)
	return nil
}

