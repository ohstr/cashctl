//go:build !crashpoints

package crashpoint

// Enabled reports whether this build honours CASHCTL_CRASHPOINT and the other
// test-only overrides gated on the crashpoints tag.
const Enabled = false

func kill(string) {}
