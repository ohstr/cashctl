//go:build !windows

package cmd

import "os"

// openControllingTTY opens the process's controlling terminal so a secret
// can be read without echo independently of whatever fd 0 is wired to. On
// Unix that is /dev/tty — the same device ssh-keygen and sudo prompt on, so
// a password read still works when stdin is a pipe.
//
// Kept in a variable so a test can simulate a headless environment where no
// terminal is reachable.
var openControllingTTY = func() (*os.File, error) {
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}
