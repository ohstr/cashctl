//go:build crashpoints

package crashpoint

import "os"

// Enabled reports whether this build honours CASHCTL_CRASHPOINT and the other
// test-only overrides gated on the crashpoints tag.
const Enabled = true

func kill(name string) {
	if os.Getenv(EnvVar) != name {
		return
	}
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		_ = p.Kill()
	}
	select {} // the kill is asynchronous on some platforms: never run past the point
}
