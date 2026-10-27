//go:build !crashpoints

package crashpoint

import "testing"

// A release build never dies at a crash point, whatever the environment says.
func TestHit_IgnoresTheEnvironmentWithoutTheTag(t *testing.T) {
	t.Setenv(EnvVar, "x")
	Hit("x")
	if Enabled {
		t.Error("Enabled is true without the crashpoints tag")
	}
}
