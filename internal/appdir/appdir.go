// Package appdir resolves cashctl's local state directory — where
// cashctl.db (see internal/store) lives.
package appdir

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// override is set once, at startup, by cmd/root.go's --config-dir flag —
// every package that persists local state reads it through Dir rather
// than each reimplementing the override.
var override string

// notices is where Dir reports tightening a loose directory (stderr, like
// all narration); a test swaps it.
var notices io.Writer = os.Stderr

// SetOverride sets a --config-dir override for the process lifetime.
func SetOverride(dir string) { override = dir }

// Dir returns cashctl's local config directory: the --config-dir override if
// set, otherwise the OS-appropriate per-user config directory ($XDG_CONFIG_HOME
// or ~/.config on Linux, via os.UserConfigDir) joined with "cashctl". Created
// (mode 0700, since everything under it can hold spendable secrets — see
// cashctl-plan.md's Local wallet layer) if it doesn't exist yet, and
// tightened to 0700 if it already exists open to group or others.
func Dir() (string, error) {
	dir := override
	if dir == "" {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(base, "cashctl")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := tighten(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// tighten makes an existing state directory private: MkdirAll leaves the
// mode of one that already exists alone, so a directory created earlier as
// 0755 (by hand, or by another tool) would keep exposing cashctl.db's
// listing and any file created in it without a mode of its own.
func tighten(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 == 0 {
		return nil
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("make %s private (it holds spendable secrets): %w", dir, err)
	}
	fmt.Fprintf(notices, "Made %s private (was %04o, now 0700): it holds spendable secrets.\n", dir, info.Mode().Perm())
	return nil
}
