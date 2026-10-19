package cmd

import (
	"encoding/json"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
)

// `init --json` says whether the identity was created or already there, and a
// second run names the identity it kept instead of a bare already_configured.
func TestWalletInit_SaysCreatedThenExisting(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })

	run := func() map[string]any {
		t.Helper()
		var err error
		stdout, _ := captureStreams(t, func() { err = runWalletInit(testCmdWithFlags(true, true), nil) })
		if err != nil {
			t.Fatalf("init: %v", err)
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(stdout), &got); err != nil {
			t.Fatalf("init stdout is not JSON: %v\n%s", err, stdout)
		}
		return got
	}
	first := run()
	if first["identity"] != "created" || first["npub"] == "" {
		t.Errorf("first init = %v, want identity created with an npub", first)
	}
	second := run()
	if second["identity"] != "existing" || second["npub"] != first["npub"] || second["already_configured"] != true {
		t.Errorf("second init = %v, want identity existing, the same npub %v", second, first["npub"])
	}
}

// I-38: with a default wallet already registered and none offered now, init
// reports the default in effect instead of "".
func TestWalletInit_ReportsTheDefaultAlreadyInEffect(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Add("r1", "nostr+walletconnect://a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1a1"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetDefault("r1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	var runErr error
	stdout, _ := captureStreams(t, func() { runErr = runWalletInit(testCmdWithFlags(true, true), nil) })
	if runErr != nil {
		t.Fatalf("init: %v", runErr)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("init stdout is not JSON: %v\n%s", err, stdout)
	}
	if got["default_wallet"] != "r1" {
		t.Errorf("default_wallet = %v, want r1", got["default_wallet"])
	}
}
