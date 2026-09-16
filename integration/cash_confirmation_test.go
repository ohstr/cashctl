//go:build integration

package integration

import (
	"strings"
	"testing"
)

// This file exercises real interactive (text-mode, no --json)
// confirmation prompts via fixture.runInteractive — every other test in
// this suite passes --json, which skips them.

// TestCashRedeem_BareEnterDeclinesByDefault: redeem's confirmation must
// default to NO on a bare Enter — never accept a send/spend passively.
func TestCashRedeem_BareEnterDeclinesByDefault(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 20_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("connect", "add", "hub", hub.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// A bare Enter: empty stdin line, no "y"/"yes" typed.
	res := f.runInteractive("\n", "redeem", "hub")
	if res.ExitCode != 0 {
		t.Fatalf("redeem (bare Enter): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Cancelled") {
		t.Fatalf("redeem (bare Enter): expected \"Cancelled\", got stdout: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "[y/N]") {
		t.Errorf("redeem confirmation prompt = %q, want it to show [y/N] (default no)", res.Stdout)
	}

	if n := heldCount(t, f); n != 1 {
		t.Fatalf("a declined redeem must leave the token held, got %d held", n)
	}
}

// TestCashRedeem_ExplicitYAcceptsDespiteDefaultNo confirms the flip to
// default-no didn't just make redeem impossible interactively — typing
// "y" explicitly must still redeem for real.
func TestCashRedeem_ExplicitYAcceptsDespiteDefaultNo(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, 15_000)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("connect", "add", "hub", hub.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	res := f.runInteractive("y\n", "redeem", "hub")
	if res.ExitCode != 0 {
		t.Fatalf("redeem (explicit y): exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "Redeemed") {
		t.Errorf("redeem (explicit y): expected a success message, got stdout: %q", res.Stdout)
	}
	if n := heldCount(t, f); n != 0 {
		t.Errorf("an accepted redeem must consume the held token, got %d still held", n)
	}
}

// TestCashRedeem_PreviewShowsExpiryWarning confirms redeem's confirmation
// shows an expiry warning up front, not only after redeeming.
// setUpCashHub's CashMaxExpSecs (1h) is within the "soon" threshold.
func TestCashRedeem_PreviewShowsExpiryWarning(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	initResp := f.mustJSON("wallet", "init")
	myPubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	const amountMillis = uint64(30_000)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	if res := f.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("connect", "add", "hub", hub.PairingUri); res.ExitCode != 0 {
		t.Fatalf("connect add: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Decline (bare Enter) — this test only cares about what the prompt
	// itself said, not about actually redeeming.
	res := f.runInteractive("\n", "redeem", "hub")
	if res.ExitCode != 0 {
		t.Fatalf("redeem (preview check): unexpected exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !strings.Contains(res.Stdout, "expires in") && !strings.Contains(res.Stdout, "deadline has already passed") {
		t.Errorf("redeem confirmation: expected an expiry warning (this hub's own CashMaxExpSecs is 1h), got stdout: %q", res.Stdout)
	}
	if !strings.Contains(res.Stdout, "30000") {
		t.Errorf("redeem confirmation: expected the token's own amount (30000) shown before confirming, got stdout: %q", res.Stdout)
	}

	if n := heldCount(t, f); n != 1 {
		t.Fatalf("a declined redeem must leave the token held, got %d held", n)
	}
}
