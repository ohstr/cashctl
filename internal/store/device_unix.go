//go:build unix

package store

import (
	"os"
	"syscall"
)

// fileID is the file's device and inode numbers.
func fileID(path string) (dev, ino uint64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, err
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Dev), st.Ino, nil //nolint:unconvert // Dev's type differs by platform
	}
	return 0, 0, nil
}
