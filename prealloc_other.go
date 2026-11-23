//go:build !linux || android

package main

import "os"

// preallocateFile is a safe no-op fallback on Windows, Android, and other platforms.
func preallocateFile(f *os.File, size int64) error {
	return nil
}

