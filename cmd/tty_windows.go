//go:build windows

package cmd

import "os"

// openControllingTTY opens the console input buffer (CONIN$) so a secret
// can be read without echo independently of whatever fd 0 is wired to —
// the Windows equivalent of Unix's /dev/tty, so a password read still works
// when stdin is a pipe.
//
// Kept in a variable so a test can simulate a headless environment where no
// terminal is reachable.
var openControllingTTY = func() (*os.File, error) {
	return os.OpenFile("CONIN$", os.O_RDWR, 0)
}
