//go:build integration

package integration

import (
	"strings"
	"testing"
)

// Edge cases in `transfer` found by the campaign's E3 executor and
// re-verified here against the live hub.

// A bearer gift is only spendable by someone who is handed the token AND its
// secret. When the whole token is transferred (a full transfer reassigns the
// wallet IN PLACE, so there is no new token), the command must still hand the
// sender the complete "<token>#<secret>" string — in --json as cash_to_send,
// and in text mode on screen. Today it prints neither, so the sender has
// given the money to a secret nobody was told.
func TestTransfer_FullTokenToBearerTarget_GiftStringIsHandedBack(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	tokenJSON := mintPubkeyTokenFromHub(t, hub, pub, 30_000)
	f.mustJSON("receive", tokenJSON)
	out := f.mustJSON("transfer", "cash", "--yes")
	if gift, _ := out["cash_to_send"].(string); !strings.Contains(gift, "#") {
		t.Errorf("--json: a full transfer to cash returned no cash_to_send gift string (new_wallet_token=%q); the secret is only inside target_resolved prose: %v",
			out["new_wallet_token"], out)
	}

	tokenText := mintPubkeyTokenFromHub(t, hub, pub, 30_000)
	f.mustJSON("receive", tokenText)
	res := f.runInteractive("y\n", "transfer", "cash")
	if res.ExitCode != 0 {
		t.Fatalf("text transfer: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !hex64Re.MatchString(res.Stdout) {
		t.Errorf("text mode: transferred the whole token to a bearer target but never showed the secret (or any gift string) — the recipient can't be given anything:\n%s", res.Stdout)
	}
}

// `transfer 0 <target>` / `--amount 0` must not mean "everything". Zero is
// not a valid amount to send; the prompt even claims the full amount.
func TestTransfer_ZeroAmountIsRejected_NotTreatedAsWholeToken(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 10_000))

	for _, args := range [][]string{
		{"transfer", fakeHex32(t), "0", "--yes"},
		{"transfer", fakeHex32(t), "--amount", "0", "--yes"},
		{"transfer", fakeHex32(t), "0.000", "--yes"},
	} {
		res := f.run(args...)
		if res.ExitCode == 0 {
			t.Errorf("cashctl %v exited 0 — a zero amount sent the whole token:\n%s", args[:1], res.Stdout)
			break // the token is gone; later iterations would only fail for lack of funds
		}
	}
}

// A split remainder is a new token; it must keep the mint-signature identity
// the source had, or cash selection can no longer group it with same-minter
// holdings and reports a false "funds are fragmented across separate Hubs".
func TestTransfer_SplitRemainderKeepsMinterForCashSelection(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	tokA, _, okA := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 10_000)
	tokB, _, okB := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 6_000)
	if !okA || !okB {
		t.Skip("this Hub did not attach a mint signature — same-minter grouping can't be exercised")
	}
	f.mustJSON("receive", tokA)
	f.mustJSON("receive", tokB)

	f.mustJSON("transfer", fakeHex32(t), "8", "--yes") // splits the 10 -> remainder 2 (+ the untouched 6)
	res := f.run("transfer", fakeHex32(t), "7", "--yes")
	if res.ExitCode != 0 {
		t.Errorf("7 loki is available from same-minter holdings (remainder 2 + 6) but transfer failed: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
}

// When a multi-step `consolidate` fails after the Hub has already merged the
// sources, the local ledger must not keep listing the consumed sources as
// held (they can no longer be spent, and balance counts them twice over).
func TestConsolidate_FailureDoesNotLeaveConsumedSourcesHeld(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	type src struct{ id, token string }
	var srcs []src
	for i := 0; i < 2; i++ {
		entry := f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 30_000))["entry"].(map[string]any)
		srcs = append(srcs, src{entryID(t, entry), entry["token"].(string)})
	}

	res := f.run("consolidate", "--sources", srcs[0].id+","+srcs[1].id, "--to", "cash", "--yes")
	if res.ExitCode == 0 {
		t.Skip("consolidate to a bearer target succeeded on this hub; the partial-failure path can't be exercised")
	}
	for _, s := range srcs {
		// decode --check asks the Hub whether this token still has a live
		// recipient for us — the truth about whether it can be spent.
		check, _ := f.mustJSON("decode", s.token, "--check")["check"].(map[string]any)
		stillLive, _ := check["ok"].(bool)
		heldLocally := false
		heldTokens, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any) // nil when none held — see the null-vs-[] finding
		for _, h := range heldTokens {
			if e, _ := h.(map[string]any); e["id"] == s.id {
				heldLocally = true
			}
		}
		if heldLocally && !stillLive {
			t.Errorf("after a failed consolidate (exit %d) source %s is still listed as held but the Hub says it is gone — the ledger counts money that has moved",
				res.ExitCode, s.id)
		}
	}
}

// `transfer <amount>` with NO target defaults to a bearer note (see
// runCashTransfer's own doc comment: "just an amount, share the result with
// whoever"). When that amount happens to equal the whole held token, the
// call takes the SAME full-transfer-to-cash path as
// TestTransfer_FullTokenToBearerTarget_GiftStringIsHandedBack above — so it
// must hand back the same gift string. A partial amount already does; only
// the exact-whole-token case is at risk of silently reusing the "amount
// omitted" full-transfer code path with nothing to show for it.
func TestTransfer_AmountOnlyEqualToWholeToken_StillReturnsGift(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)
	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 1_000)) // exactly 1 loki

	out := f.mustJSON("transfer", "1", "--yes")
	if gift, _ := out["cash_to_send"].(string); !strings.Contains(gift, "#") {
		t.Errorf("`transfer 1` on a 1-loki token (amount == whole token, no explicit target) returned no cash_to_send: %v", out)
	}
	if n := heldCount(t, f); n != 0 {
		t.Errorf("held tokens after the transfer = %d, want 0", n)
	}
}
