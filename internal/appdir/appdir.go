// Package appdir resolves cashctl's local state directory — where
// cashctl.db (see internal/store) lives.
package appdir

import (
	"os"
	"path/filepath"
)

// override is set once, at startup, by cmd/root.go's --config-dir flag —
// every package that persists local state reads it through Dir rather
// than each reimplementing the override.
var override string

// SetOverride sets a --config-dir override for the process lifetime.
func SetOverride(dir string) { override = dir }

// Dir returns cashctl's local config directory: the --config-dir override if
// set, otherwise the OS-appropriate per-user config directory ($XDG_CONFIG_HOME
// or ~/.config on Linux, via os.UserConfigDir) joined with "cashctl". Created
// (mode 0700, since everything under it can hold spendable secrets — see
// cashctl-plan.md's Local wallet layer) if it doesn't exist yet.
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
	return dir, nil
}
