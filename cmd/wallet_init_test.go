package cmd

import (
	"encoding/json"
	"testing"

	"github.com/ohstr/cashctl/internal/appdir"
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
