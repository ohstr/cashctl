package cmd

import (
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/store"
)

// CASHCTL_POW_MAX sets the proof-of-work cap; an unreadable value never
// lifts it.
func TestPowMax(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	for v, want := range map[string]int{"": defaultPowMax, "20": 20, "1": 1} {
		t.Setenv(powMaxEnv, v)
		if got, err := powMax(); err != nil || got != want {
			t.Errorf("%q: %d %v, want %d", v, got, err, want)
		}
	}
	for _, bad := range []string{"x", "-1", "0", "65"} {
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

// The cap's precedence: --pow-max, then CASHCTL_POW_MAX, then the
// pow_max_difficulty setting, then 28.
func TestPowMax_Precedence(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	t.Setenv(powMaxEnv, "")
	if n, _ := powMax(); n != defaultPowMax {
		t.Errorf("default %d", n)
	}
	if err := store.SetSetting(powMaxSetting, "20"); err != nil {
		t.Fatal(err)
	}
	if n, _ := powMax(); n != 20 {
		t.Errorf("setting: %d", n)
	}
	t.Setenv(powMaxEnv, "22")
	if n, _ := powMax(); n != 22 {
		t.Errorf("env over setting: %d", n)
	}
	f := RootCmd.PersistentFlags().Lookup("pow-max")
	prev := f.Value.String()
	_ = RootCmd.PersistentFlags().Set("pow-max", "24")
	t.Cleanup(func() { _ = f.Value.Set(prev); f.Changed = false })
	if n, _ := powMax(); n != 24 {
		t.Errorf("flag over env: %d", n)
	}
}

// config set refuses an unknown key and a bad difficulty.
func TestConfigCmd_SetsPowMax(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	run := func(args ...string) error {
		c := newConfigCmd()
		c.PersistentFlags().Bool("json", true, "")
		c.SetArgs(args)
		var err error
		captureStreams(t, func() { err = c.Execute() })
		return err
	}
	if err := run("set", "pow_max_difficulty", "65"); output.ExitCode(err) != 3 {
		t.Errorf("65: %v", err)
	}
	if err := run("set", "nope", "1"); output.ExitCode(err) != 3 {
		t.Errorf("unknown key: %v", err)
	}
	if err := run("set", "pow_max_difficulty", "16"); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := store.Setting(powMaxSetting); !ok || v != "16" {
		t.Errorf("stored %q %v", v, ok)
	}
	_ = run("unset", "pow_max_difficulty")
	if _, ok, _ := store.Setting(powMaxSetting); ok {
		t.Error("unset kept it")
	}
}
