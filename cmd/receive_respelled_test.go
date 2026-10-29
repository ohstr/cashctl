package cmd

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// countingRelay is a relay address that counts connections and closes them.
func countingRelay(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			n.Add(1)
			_ = c.Close()
		}
	}()
	return "ws://" + ln.Addr().String(), &n
}

func TestReceive_ABillHeldUnderAnotherSpellingIsAConflictBeforeAnyDial(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	relay, dialled := countingRelay(t)
	bill := testmint.Bill(t, nipcash.Token{RelayURLs: []string{relay}}, "")
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Add(ledger.Entry{Token: bill}); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	receive := func(s string) error {
		cmd := &cobra.Command{}
		cmd.Flags().Bool("json", true, "")
		cmd.Flags().Bool("yes", true, "")
		var runErr error
		withCapturedOutput(func() { runErr = runCashReceive(cmd, []string{s}) })
		return runErr
	}
	for name, s := range map[string]string{
		"an unknown TLV appended": testmint.WithUnknownTLV(t, bill),
		"entries reordered":       testmint.Reordered(t, bill),
		"uppercase":               strings.ToUpper(bill),
	} {
		if err := receive(s); output.ExitCode(err) != 5 {
			t.Errorf("%s: exit %d (%v), want 5 (conflict)", name, output.ExitCode(err), err)
		}
	}
	if n := dialled.Load(); n != 0 {
		t.Errorf("the bill's relay was contacted %d time(s) for a bill already held", n)
	}

	// Another bill on the same relay is checked with its Hub as usual.
	other := testmint.Bill(t, nipcash.Token{RelayURLs: []string{relay}}, "")
	if err := receive(other); output.ExitCode(err) == 5 {
		t.Fatalf("a different bill was refused as already held: %v", err)
	}
	if dialled.Load() == 0 {
		t.Error("a different bill was never checked with its Hub")
	}
	after, _ := ledger.Load()
	if n := len(after.Held()); n != 1 {
		t.Errorf("held = %d, want 1 (the unchecked bill is not saved)", n)
	}
}
