//go:build !crashpoints

package cmd

import "github.com/ohstr/nmilat/nip01"

// publishFilter drops nothing in a release build: only a crashpoints build may
// lose a publish on purpose.
func publishFilter() func(*nip01.Event) bool { return nil }
