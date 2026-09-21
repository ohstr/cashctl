//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
)

// -c/--connection is a global flag, so cobra accepts it on every command —
// but on commands that never use a registered wallet it used to do nothing,
// silently. A script that names a wallet expects that wallet to be involved;
// failing fast (before anything runs) beats letting the expectation go
// unnoticed.
func TestConnectionFlag_RejectedWhereItCannotApply(t *testing.T) {
	f := newFixture(t)
	for _, args := range [][]string{
		{"-c", "savings", "receive", "lokicash1notatoken"},
		{"-c", "savings", "transfer", "1", fakeHex32(t)},
		{"-c", "savings", "consolidate"},
		{"-c", "savings", "decode", "lokicash1notatoken"},
		{"-c", "savings", "cash", "list-recipients"},
	} {
		res := f.run(args...)
		if res.ExitCode != 2 {
			t.Errorf("cashctl %v: exit %d, want 2 (usage)\nstdout: %s\nstderr: %s", args, res.ExitCode, res.Stdout, res.Stderr)
			continue
		}
		if res.Stdout != "" {
			t.Errorf("cashctl %v: stdout must stay empty on a usage error, got %q", args, res.Stdout)
		}
		if !strings.Contains(res.Stderr, "-c/--connection") {
			t.Errorf("cashctl %v: the error should name the flag it's rejecting:\n%s", args, res.Stderr)
		}
	}
}

// Without -c the same commands are untouched (this pins that the guard keys
// off the flag, not the command).
func TestConnectionFlag_AbsentMeansNoChange(t *testing.T) {
	f := newFixture(t)
	res := f.run("decode", "lokicash1notatoken")
	if strings.Contains(res.Stderr, "-c/--connection") {
		t.Errorf("decode without -c mentioned the flag: %s", res.Stderr)
	}
}

// redeem's destination is "the default wallet" unless told otherwise, so -c
// (the global "use this wallet instead of the default") must not be ignored
// there, and must never be silently combined with a conflicting destination.
func TestRedeem_ConnectionFlagConflicts_AreUsageErrors(t *testing.T) {
	f := newFixture(t)

	res := f.run("-c", "b", "redeem", "--into", "a")
	if res.ExitCode != 2 {
		t.Errorf("redeem --into a -c b: exit %d, want 2\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Same value twice is not a conflict.
	res = f.run("-c", "a", "redeem", "--into", "a")
	if res.ExitCode == 2 && strings.Contains(res.Stderr, "different values") {
		t.Errorf("--into a with -c a is the same destination, not a conflict:\n%s", res.Stderr)
	}

	invoice, err := bech32.Encode("lnbc", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
	if err != nil {
		t.Fatal(err)
	}
	res = f.run("-c", "b", "redeem", "--invoice", invoice)
	if res.ExitCode != 2 {
		t.Errorf("redeem --invoice … -c b: exit %d, want 2 (an invoice has no wallet to choose)\nstderr: %s", res.ExitCode, res.Stderr)
	}
}

// balance -c X used to be ignored: it summed every wallet and printed a total
// that looked like X's alone. It now scopes to X, exactly like --from X.
func TestBalance_ConnectionFlagScopesToOneWallet(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("connect", "add", "dead", fakeNWCURI(t))

	if res := f.run("wallet", "balance"); res.ExitCode != 0 {
		t.Fatalf("plain balance over an unreachable wallet: exit %d\n%s", res.ExitCode, res.Stderr)
	}
	// Scoped to the dead wallet the real error must surface instead of a
	// quiet "0 loki total" — proof the flag is being honored.
	if res := f.run("-c", "dead", "wallet", "balance"); res.ExitCode == 0 {
		t.Errorf("balance -c dead: exit 0 — the flag was ignored and every wallet summed instead:\n%s", res.Stdout)
	}
	if res := f.run("-c", "no-such-wallet", "wallet", "balance"); res.ExitCode != 4 {
		t.Errorf("balance -c no-such-wallet: exit %d, want 4 (not_found)\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := f.run("--json", "wallet", "balance", "--from", "x", "-c", "y"); res.ExitCode != 2 {
		t.Errorf("balance --from x -c y: exit %d, want 2 (conflicting values)", res.ExitCode)
	}
}

// TestBalance_UnreachableWalletSurfacedNotSilentlyDropped guards against
// the bug found auditing plain `balance` (no -c/--from): an unreachable
// registered wallet used to just vanish from the total and breakdown with
// zero indication — "0 loki" looked identical to "this wallet is actually
// empty." It's now named in --json's "unreachable" list and in the
// text-mode footer, never silently treated as zero.
func TestBalance_UnreachableWalletSurfacedNotSilentlyDropped(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("connect", "add", "dead", fakeNWCURI(t))

	resp := f.mustJSON("wallet", "balance", "--breakdown")
	unreachable, _ := resp["unreachable"].([]any)
	if len(unreachable) != 1 || unreachable[0] != "dead" {
		t.Errorf(`balance --json "unreachable" = %v, want ["dead"]`, resp["unreachable"])
	}
	if breakdown, _ := resp["breakdown"].([]any); len(breakdown) != 0 {
		t.Errorf("balance --json breakdown = %v, want empty — an unreachable wallet has nothing to itemize, only to name as unreachable", breakdown)
	}

	textRes := f.runInteractive("", "wallet", "balance")
	if !strings.Contains(textRes.Combined(), "dead") {
		t.Errorf("balance (text) doesn't mention the unreachable wallet by name: %s", textRes.Combined())
	}
}

// Live: the default wallet is unreachable, so if -c were ignored redeem would
// dial it and fail. It must go to the named wallet instead.
func TestRedeem_ConnectionFlagChoosesTheDestination(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	myPubHex, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, myPubHex, 45_000))

	f.mustJSON("connect", "add", "broken", fakeNWCURI(t)) // first wallet: becomes the default
	f.mustJSON("connect", "add", "payout", hub.PairingUri)
	if got := f.mustJSON("connect", "list")["default"]; got != "broken" {
		t.Fatalf("test setup: default = %v, want broken", got)
	}

	resp := f.mustJSON("-c", "payout", "redeem", "--yes")
	if preimage, _ := resp["preimage"].(string); preimage == "" {
		t.Errorf("redeem -c payout: empty preimage: %v", resp)
	}
	if got := resp["to_wallet"]; got != "payout" {
		t.Errorf("to_wallet = %v, want payout (the -c wallet, not the default)", got)
	}
}

// A destination that is neither a registered name nor a connection string can
// never work: not_found, not a retryable network failure.
func TestRedeem_UnusableDestination_IsNotRetryable(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	myPubHex, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, myPubHex, 20_000))

	for _, args := range [][]string{
		{"-c", "garbage", "redeem", "--yes"},
		{"redeem", "--into", "garbage", "--yes"},
	} {
		res := f.run(args...)
		if res.ExitCode == 0 {
			t.Errorf("cashctl %v unexpectedly succeeded", args)
			continue
		}
		if e := parseErrorReport(t, res); e.Code != "not_found" || e.Retryable {
			t.Errorf("cashctl %v: code=%q retryable=%v, want not_found and not retryable\n%s", args, e.Code, e.Retryable, res.Stderr)
		}
	}
}
