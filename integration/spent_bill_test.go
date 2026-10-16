//go:build integration

package integration

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
)

// requireBillSpentAway asserts that a bill whose value has moved away is gone
// from the Hub — read from the Hub's own records, not from the bill's
// connection.
//
// This used to dial the bill and infer destruction from what came back: a
// "spent" tombstone, or a timeout taken to mean silence. Both inferences are
// now unavailable to a holder. Bill methods are served over the private
// transport, which authorizes per recipient and omits anything it cannot serve,
// so a destroyed bill and a live bill this caller has no slice of produce the
// same non-answer. Worse, so does a Hub that is merely slow — which made
// "timed out, therefore destroyed" a check that passes when the Hub is down.
//
// The operator's view has none of those ambiguities: a bill is destroyed when
// every slice under its wallet pubkey has moved to the archive. That is the
// actual claim these tests make, stated directly.
//
// What a NORMAL client — one with no admin API — can and cannot observe is a
// real property worth testing, but it is a different property, and it belongs
// in a test of its own rather than smuggled into every test that merely needs
// to know a bill is gone. See TestSpentBill_PlainClientObservesNothing.
//
// Still retried rather than asserted once, because the Hub deletes the bill
// AFTER answering the request that emptied it: for a moment afterwards its
// slices are still live.
func requireBillSpentAway(t *testing.T, admin *adminClient, hubAppID uint, token, what string) {
	t.Helper()

	// Callers hold whatever string they were given, which for a cash-mode bill
	// carries the "#secret" suffix; only the token itself decodes.
	tokenOnly, _ := nipcash.SplitCashSliceString(token)
	tok, err := nipcash.Decode(tokenOnly)
	if err != nil {
		t.Fatalf("%s: decode token: %v", what, err)
	}

	const deleteWindow = 20 * time.Second
	deadline := time.Now().Add(deleteWindow)
	for {
		rows, err := admin.listCashWalletClaims(hubAppID)
		if err != nil {
			t.Fatalf("%s: list cash wallet claims: %v", what, err)
		}
		var known, live int
		for _, r := range rows {
			if r.WalletPubkey != tok.WalletPubkey {
				continue
			}
			known++
			if !r.Archived {
				live++
			}
		}
		if known > 0 && live == 0 {
			return
		}
		if time.Now().After(deadline) {
			if live > 0 {
				t.Fatalf("%s: %s after this bill's value moved away the Hub still holds %d live slice(s) on wallet %s — the bill was not destroyed",
					what, deleteWindow, live, tok.WalletPubkey)
			}
			t.Fatalf("%s: the Hub has no record at all, live or archived, of wallet %s — expected an archived bill",
				what, tok.WalletPubkey)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// TestSpentBill_TombstoneMarksTheEntrySpent (F-43): a holder whose bill was spent
// elsewhere learns it from the Hub's tombstone, not from silence. Two copies of
// one wallet hold the same bill; one redeems it, and the other's `cash status`
// must report it spent and drop it from what it holds.
func TestSpentBill_TombstoneMarksTheEntrySpent(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)

	a := newFixture(t)
	initResp := a.mustJSON("wallet", "init")
	pubHex, err := npubToHex(initResp["npub"].(string))
	if err != nil {
		t.Fatalf("decode npub: %v", err)
	}
	const amountMillis = uint64(300_000)
	token := mintPubkeyTokenFromHub(t, hub, pubHex, amountMillis)
	if res := a.run("receive", token); res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// b is a second copy of the same wallet, taken while the bill is still held.
	b := newFixture(t)
	if err := os.Remove(b.configDir); err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(b.configDir, os.DirFS(a.configDir)); err != nil {
		t.Fatalf("copy wallet: %v", err)
	}

	invoice := makeHubInvoice(t, hub, amountMillis)
	if res := a.run("redeem", "--invoice", invoice, "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	requireBillSpentAway(t, admin, hub.ID, token, "after redeem")

	// The tombstone is a definite answer: b's very first read after it is the
	// spent error (exit 10, stderr only), the same one every later read gets.
	bHeld, _ := b.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(bHeld) != 1 {
		t.Fatalf("the copied wallet holds %d bills, want 1", len(bHeld))
	}
	id := entryID(t, bHeld[0])
	deadline := time.Now().Add(20 * time.Second)
	for {
		res := b.run("cash", "status", "--token", id)
		if res.ExitCode == 10 {
			if e := parseErrorReport(t, res); e.Code != "spent" || e.Retryable {
				t.Errorf("first cash status on the tombstone: code=%q retryable=%v, want spent, not retryable", e.Code, e.Retryable)
			}
			if strings.TrimSpace(res.Stdout) != "" {
				t.Errorf("first cash status on the tombstone printed on stdout: %s", res.Stdout)
			}
			break
		}
		if res.ExitCode != 0 || strings.Contains(res.Stdout, `"spent"`) {
			t.Fatalf("cash status on the tombstone: exit %d, want 10 (spent)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
		}
		if time.Now().After(deadline) {
			t.Fatalf("cash status never reported the redeemed bill spent: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
		}
		time.Sleep(time.Second)
	}
	if again := b.run("cash", "status", "--token", id); again.ExitCode != 10 || parseErrorReport(t, again).Code != "spent" {
		t.Errorf("second cash status: exit %d, want 10 (spent)\nstderr: %s", again.ExitCode, again.Stderr)
	}

	show := b.mustJSON("wallet", "show")
	if held, _ := show["held_tokens"].([]any); len(held) != 0 {
		t.Errorf("wallet show still holds the spent bill: %v", held)
	}
	res := b.run("redeem", "--all")
	if res.ExitCode == 0 {
		t.Errorf("redeem --all succeeded on a wallet whose only bill is spent: %s", res.Stdout)
	}

	// A third party holding the token string gets the same answer, as code spent.
	c := newFixture(t)
	c.mustJSON("wallet", "init")
	res = c.run("receive", token)
	if res.ExitCode != 10 || !strings.Contains(res.Stderr, `"spent"`) {
		t.Errorf("receive of a spent bill: exit %d, want 10 (spent)\nstderr: %s", res.ExitCode, res.Stderr)
	}
}

// TestDecodeCheck_BareCashModeBillNamesTheMissingSecret (I-22): a Hub answers about
// a cash-mode bill only with its cash secret, so checking the bare token must say
// to pass `<token>#<secret>` rather than report a generic failure.
func TestDecodeCheck_BareCashModeBillNamesTheMissingSecret(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	gift := mintCashGift(t, hub, 50_000)
	bare, _ := nipcash.SplitCashSliceString(gift)
	resp := f.mustJSON("decode", bare, "--check")
	check, _ := resp["check"].(map[string]any)
	if check == nil {
		t.Fatalf("decode --check: no check field: %v", resp)
	}
	if ok, _ := check["ok"].(bool); ok {
		t.Fatalf("decode --check of a bare cash-mode bill matched without its secret: %v", check)
	}
	if msg, _ := check["error"].(string); !strings.Contains(msg, "<token>#<secret>") {
		t.Errorf("decode --check error does not name the missing secret: %q", msg)
	}
}
