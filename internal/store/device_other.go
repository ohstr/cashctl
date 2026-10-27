//go:build !unix

package store

// fileID has no device or inode number to read here; the machine id and the
// path still change with a copy to another machine or place.
func fileID(string) (dev, ino uint64, err error) { return 0, 0, nil }
