//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
)

// The flows in this file are CHAINS: the output of one command is spent
// again by the next. Single-step tests (a split happens; the remainder
// exists) can't see a remainder that is saved but not actually spendable —
// which is how a cash-mode split remainder, saved without its cash
// secret, shipped: the FIRST spend worked, the SECOND asked the user to run
// `cashctl init`. Every chain here spends what the previous step left.

// TestChain_CashOnlyWallet_NoIdentity_SplitsRespentThenRedeemed is the
// exact user report: a wallet that only ever held cash (no `init`,
// ever) transfers 1 loki several times in a row — each split's remainder
// must itself be spendable with the same secret — then redeems what's left.
func TestChain_CashOnlyWallet_NoIdentity_SplitsRespentThenRedeemed(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t) // deliberately NO `wallet init`

	const start = uint64(60_000) // 60 loki
	gift := mintCashGift(t, hub, start)

	res := f.run("receive", gift, "--yes")
	assertNoIdentityDemand(t, "receive", res)
	if res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	remaining := start
	for i := 1; i <= 3; i++ {
		step := fmt.Sprintf("transfer 1 (#%d)", i)
		res := f.run("transfer", "1", "--yes") // amount only -> cash
		assertNoIdentityDemand(t, step, res)
		if res.ExitCode != 0 {
			t.Fatalf("%s: exit %d\nstdout: %s\nstderr: %s", step, res.ExitCode, res.Stdout, res.Stderr)
		}
		out := mustDecodeJSON(t, step, res.Stdout)
		remaining -= 1000
		if got := mloki(out, "remaining_amount_millis"); got != remaining {
			t.Fatalf("%s: remaining_amount_millis = %d, want %d", step, got, remaining)
		}
		if gift, _ := out["cash_to_send"].(string); !strings.Contains(gift, "#") {
			t.Errorf("%s: cash_to_send = %q, want a token#secret gift string", step, gift)
		}
	}

	// The last remainder is redeemable, too — same secret, no identity.
	inv := makeHubInvoice(t, hub, remaining)
	res = f.run("redeem", "--invoice", inv, "--yes")
	assertNoIdentityDemand(t, "redeem remainder", res)
	if res.ExitCode != 0 {
		t.Fatalf("redeem remainder: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if pre, _ := mustDecodeJSON(t, "redeem", res.Stdout)["preimage"].(string); pre == "" {
		t.Errorf("redeem remainder: no preimage in %s", res.Stdout)
	}

	// History needs no identity either, and must record every step.
	hist := f.run("wallet", "history")
	if hist.ExitCode != 0 {
		t.Fatalf("wallet history: exit %d\nstderr: %s", hist.ExitCode, hist.Stderr)
	}
	if n := strings.Count(hist.Stdout, "transferred"); n < 3 {
		t.Errorf("wallet history lists %d transfers, want at least 3: %s", n, hist.Stdout)
	}
}

// TestChain_CashRemainder_TransferredOnwardToSecondUser: a cash-mode split
// leaves a remainder; that remainder is then sent (split again) to a real
// second cashctl user by pubkey, who receives and redeems it; the sender
// redeems what's left of the remainder.
func TestChain_CashRemainder_TransferredOnwardToSecondUser(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)

	a := newFixture(t) // cash-mode-only, no identity
	b := newFixture(t)
	bHex, err := npubToHex(b.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatalf("decode B's npub: %v", err)
	}

	const start = uint64(60_000)
	a.mustJSON("receive", mintCashGift(t, hub, start), "--yes")

	first := a.mustJSON("transfer", "1", "--yes") // creates the remainder
	afterFirst := mloki(first, "remaining_amount_millis")
	if afterFirst != start-1000 {
		t.Fatalf("first transfer remaining = %d, want %d", afterFirst, start-1000)
	}

	res := a.run("transfer", bHex, "5", "--yes") // spends the remainder onward
	assertNoIdentityDemand(t, "transfer remainder to B", res)
	if res.ExitCode != 0 {
		t.Fatalf("transfer remainder to B: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	toB := mustDecodeJSON(t, "transfer to B", res.Stdout)
	tokenForB, _ := toB["new_wallet_token"].(string)
	if tokenForB == "" {
		t.Fatalf("transfer to B: no new_wallet_token for a split: %v", toB)
	}
	afterSecond := mloki(toB, "remaining_amount_millis")
	if afterSecond != afterFirst-5000 {
		t.Fatalf("second transfer remaining = %d, want %d", afterSecond, afterFirst-5000)
	}

	bRecv := b.mustJSON("receive", tokenForB)
	if got := mloki(bRecv["entry"].(map[string]any), "amount_millis"); got != 5000 {
		t.Errorf("B received %d mloki, want 5000", got)
	}
	bRedeem := b.mustJSON("redeem", "--invoice", makeHubInvoice(t, hub, 5000), "--yes")
	if pre, _ := bRedeem["preimage"].(string); pre == "" {
		t.Errorf("B redeem: no preimage: %v", bRedeem)
	}

	aRedeem := a.run("redeem", "--invoice", makeHubInvoice(t, hub, afterSecond), "--yes")
	assertNoIdentityDemand(t, "A redeems what is left", aRedeem)
	if aRedeem.ExitCode != 0 {
		t.Fatalf("A redeem leftover: exit %d\nstdout: %s\nstderr: %s", aRedeem.ExitCode, aRedeem.Stdout, aRedeem.Stderr)
	}
}

// TestChain_PubkeySplitRemainder_RespentThenRedeemed: same chain in pubkey
// mode (the mode that always worked, by accident of falling through to the
// local identity) — locks that behavior in.
func TestChain_PubkeySplitRemainder_RespentThenRedeemed(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 60_000))

	one := f.mustJSON("transfer", fakeHex32(t), "10", "--yes")
	if got := mloki(one, "remaining_amount_millis"); got != 50_000 {
		t.Fatalf("first split remaining = %d, want 50000", got)
	}
	two := f.mustJSON("transfer", fakeHex32(t), "10", "--yes") // spends the remainder
	if got := mloki(two, "remaining_amount_millis"); got != 40_000 {
		t.Fatalf("second split remaining = %d, want 40000", got)
	}
	rd := f.mustJSON("redeem", "--invoice", makeHubInvoice(t, hub, 40_000), "--yes") // and the remainder of that
	if pre, _ := rd["preimage"].(string); pre == "" {
		t.Errorf("redeem: no preimage: %v", rd)
	}
	if n := heldCount(t, f); n != 0 {
		t.Errorf("held tokens after redeeming the last remainder = %d, want 0", n)
	}
}

// TestChain_ConsolidateOutput_ThenSplitTransfer_ThenRedeem: the merged token
// consolidate creates must itself be splittable, and its remainder spendable.
func TestChain_ConsolidateOutput_ThenSplitTransfer_ThenRedeem(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	id1 := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))["entry"])
	id2 := entryID(t, f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))["entry"])

	cons := f.mustJSON("consolidate", "--sources", id1+","+id2, "--yes")
	merged, _ := cons["new_entry"].(map[string]any)
	if got := mloki(merged, "amount_millis"); got != 60_000 {
		t.Fatalf("consolidated amount = %d, want 60000: %v", got, cons)
	}

	split := f.mustJSON("transfer", fakeHex32(t), "20", "--yes") // splits the merged token
	if got := mloki(split, "remaining_amount_millis"); got != 40_000 {
		t.Fatalf("split of consolidated token remaining = %d, want 40000", got)
	}
	rd := f.mustJSON("redeem", "--invoice", makeHubInvoice(t, hub, 40_000), "--yes")
	if pre, _ := rd["preimage"].(string); pre == "" {
		t.Errorf("redeem of consolidated remainder: no preimage: %v", rd)
	}
}

// TestChain_CashToPubkeyToCash_RoundTrip: cash changes mode as it moves —
// pubkey -> cash gift -> (B protects it) -> B sends it back to A's pubkey
// -> A redeems it — with the same wallet identity used throughout.
func TestChain_CashToPubkeyToCash_RoundTrip(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	a, b := newFixture(t), newFixture(t)
	aHex, err := npubToHex(a.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	b.mustJSON("wallet", "init")

	const amount = uint64(40_000)
	orig := mintPubkeyTokenFromHub(t, hub, aHex, amount)
	a.mustJSON("receive", orig)

	gifted := a.mustJSON("transfer", "cash", "--yes") // whole token, pubkey -> cash
	secret := extractHexSecret(t, gifted["target_resolved"].(string))
	gift := recipientTokenFromTransfer(gifted, orig) + "#" + secret

	bRecv := b.mustJSON("receive", gift) // auto-protect re-keys it
	if st, _ := bRecv["secured"].(map[string]any); st["status"] != "rekeyed" {
		t.Errorf("B's protect status = %v, want rekeyed", st)
	}
	bEntry, _ := bRecv["entry"].(map[string]any)
	bToken, _ := bEntry["token"].(string)

	back := b.mustJSON("transfer", aHex, "--yes") // whole token, cash -> A's pubkey
	aToken := recipientTokenFromTransfer(back, bToken)
	got := a.mustJSON("receive", aToken)
	if amt := mloki(got["entry"].(map[string]any), "amount_millis"); amt != amount {
		t.Errorf("A received %d mloki back, want %d", amt, amount)
	}
	rd := a.mustJSON("redeem", "--invoice", makeHubInvoice(t, hub, amount), "--yes")
	if pre, _ := rd["preimage"].(string); pre == "" {
		t.Errorf("A redeem: no preimage: %v", rd)
	}
}

// TestChain_TokenReturnsToOriginalHolder_CanBeReceivedAgain: a full transfer
// reassigns a wallet IN PLACE (same token string). If the token later comes
// back to its first holder, that holder must be able to `receive` it again
// even though their ledger still lists the old, transferred entry.
func TestChain_TokenReturnsToOriginalHolder_CanBeReceivedAgain(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	a, b := newFixture(t), newFixture(t)
	aHex, err := npubToHex(a.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	bHex, err := npubToHex(b.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	orig := mintPubkeyTokenFromHub(t, hub, aHex, 25_000)
	a.mustJSON("receive", orig)
	toB := a.mustJSON("transfer", bHex, "--yes") // whole token, in place
	tokForB := recipientTokenFromTransfer(toB, orig)
	b.mustJSON("receive", tokForB)
	toA := b.mustJSON("transfer", aHex, "--yes") // and back again
	tokForA := recipientTokenFromTransfer(toA, tokForB)

	res := a.run("receive", tokForA)
	if res.ExitCode != 0 {
		t.Fatalf("A re-receiving a token that came back to A: exit %d — a previously transferred entry must not block it\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestReceive_CashGiftAlreadyProtectedByAnotherHolder_IsNotSavedAsHeld: the
// same gift string reaches two people (forwarded, or an old secret kept
// around). The first holder's receive protects it (re-keys it), which kills
// the original secret. The second receive can then never spend anything, so
// it must not be saved as a healthy held token ("Verified", counted in the
// ledger) — at minimum it must be flagged, ideally refused.
func TestReceive_CashGiftAlreadyProtectedByAnotherHolder_IsNotSavedAsHeld(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	gift := mintCashGift(t, hub, 30_000)

	first := newFixture(t)
	first.mustJSON("wallet", "init")
	if st, _ := first.mustJSON("receive", gift)["secured"].(map[string]any); st["status"] != "rekeyed" {
		t.Fatalf("first holder's protect status = %v, want rekeyed", st)
	}

	second := newFixture(t)
	second.mustJSON("wallet", "init")
	res := second.run("receive", gift)
	out := res.Stdout
	held := heldCount(t, second)
	if res.ExitCode == 0 && held == 1 {
		// Saved and reported as a success. That is only acceptable if the
		// result says loudly that the secret is dead.
		body := mustDecodeJSON(t, "second receive", out)
		sec, _ := body["secured"].(map[string]any)
		if sec["likely_wrong_secret"] != true {
			t.Errorf("second receive of an already-protected gift succeeded and stored a 'held' token with no warning: %s", out)
		}
	}
}
