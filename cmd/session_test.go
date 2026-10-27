package cmd

import "testing"

// CASHCTL_POW_MAX sets the proof-of-work cap; an unreadable value never
// lifts it.
func TestPowMax(t *testing.T) {
	for v, want := range map[string]int{"": defaultPowMax, "20": 20, "0": 0} {
		t.Setenv(powMaxEnv, v)
		if got, err := powMax(); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", v, got, err, want)
		}
	}
	for _, bad := range []string{"x", "-1", "65"} {
		t.Setenv(powMaxEnv, bad)
		if _, err := powMax(); err == nil {
			t.Errorf("%q accepted", bad)
		}
		if mustPowMax() != defaultPowMax {
			t.Errorf("%q lifted the cap", bad)
		}
	}
	if len(sessionOptions()) != 1 {
		t.Error("no proof-of-work budget on the session")
	}
}
