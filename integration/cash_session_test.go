//go:build integration

package integration

import (
	"testing"
)

// historyActions extracts the ordered "action" field of every wallet
// history entry.
func historyActions(t *testing.T, f *fixture) []string {
	t.Helper()
	resp := f.mustJSON("wallet", "history")
	history, _ := resp["history"].([]any)
	out := make([]string, len(history))
	for i, h := range history {
		e, _ := h.(map[string]any)
		out[i], _ = e["action"].(string)
	}
	return out
}

// TestSession_FullLifecycle_HistoryCoherentAcrossMixedOps drives one
// cashctl identity through a realistic single sitting: join a circle for a
// Lightning wallet, receive several cash gifts, redeem one, send one
// onward, tidy up the rest with consolidate, then redeem that too — and
// confirms `wallet history` reads back as one coherent, correctly-ordered
// log spanning every action kind, not just whatever the most recent single
// command left behind. No existing test chains more than 2 cash actions
// together in one session.
func TestSession_FullLifecycle_HistoryCoherentAcrossMixedOps(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	u := newFixture(t)
	uInit := u.mustJSON("wallet", "init")
	uPub, err := npubToHex(uInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	circleHub := setUpCircleHub(t, admin, uPub)
	if circleHub.CircleHubToken == nil || *circleHub.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", circleHub)
	}
	joinResp := u.mustJSON("join", "--hub", *circleHub.CircleHubToken, "--max-amount", "100", "--yes")
	walletName, _ := joinResp["wallet"].(string)
	if walletName == "" {
		t.Fatalf("join: no wallet name in response: %v", joinResp)
	}

	cashHub := setUpCashHub(t, admin)
	const amount1, amount2, amount3, amount4 = uint64(10_000), uint64(20_000), uint64(15_000), uint64(30_000)
	var id1, id2, id3, id4 string
	for _, spec := range []struct {
		amount uint64
		idOut  *string
	}{{amount1, &id1}, {amount2, &id2}, {amount3, &id3}, {amount4, &id4}} {
		tok := mintPubkeyTokenFromHub(t, cashHub, uPub, spec.amount)
		resp := u.mustJSON("receive", tok)
		entry, _ := resp["entry"].(map[string]any)
		*spec.idOut, _ = entry["id"].(string)
		if *spec.idOut == "" {
			t.Fatalf("receive (%d): no entry id in response: %v", spec.amount, resp)
		}
	}

	// redeem #1: the smallest token, straight into the circle wallet (the
	// default — no destination needed).
	if res := u.run("redeem", "--token", id1, "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem (1st): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// transfer: send the second token onward in full.
	if res := u.run("transfer", fakeHex32(t), "--token", id2, "--yes"); res.ExitCode != 0 {
		t.Fatalf("transfer: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// consolidate: merge the remaining two dust tokens into one.
	consolidateResp := u.mustJSON("consolidate", "--sources", id3+","+id4, "--yes")
	newEntry, _ := consolidateResp["new_entry"].(map[string]any)
	newID, _ := newEntry["id"].(string)
	if newID == "" {
		t.Fatalf("consolidate: no new_entry.id in response: %v", consolidateResp)
	}
	if amt, _ := newEntry["amount_millis"].(float64); uint64(amt) != amount3+amount4 {
		t.Errorf("consolidate: new_entry.amount_millis = %v, want %d", newEntry["amount_millis"], amount3+amount4)
	}

	// redeem #2: the freshly consolidated token, into the same wallet.
	if res := u.run("redeem", "--token", newID, "--yes"); res.ExitCode != 0 {
		t.Fatalf("redeem (2nd, consolidated): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	// Nothing should be left held at all: 1 redeemed directly, 1
	// transferred away, 2 merged-then-redeemed.
	if n := heldCount(t, u); n != 0 {
		t.Fatalf("expected 0 held tokens at the end of the session, got %d", n)
	}

	// wallet balance must reflect exactly one live wallet (the circle
	// wallet both redeems landed in) and no held-token lines, self-
	// consistent with its own total.
	balanceResp := u.mustJSON("wallet", "balance", "--breakdown")
	breakdown, _ := balanceResp["breakdown"].([]any)
	var walletLines, heldLines int
	var walletAmount float64
	for _, line := range breakdown {
		l, _ := line.(map[string]any)
		switch l["kind"] {
		case "wallet":
			walletLines++
			walletAmount, _ = l["amount_mloki"].(float64)
			if name, _ := l["name"].(string); name != walletName {
				t.Errorf("wallet balance breakdown: unexpected wallet name %q, want %q", name, walletName)
			}
		case "held_token":
			heldLines++
		}
	}
	if walletLines != 1 {
		t.Fatalf("wallet balance breakdown: expected exactly 1 wallet line, got %d: %v", walletLines, breakdown)
	}
	if heldLines != 0 {
		t.Errorf("wallet balance breakdown: expected 0 held-token lines, got %d: %v", heldLines, breakdown)
	}
	if walletAmount <= 0 || walletAmount > float64(amount1+amount3+amount4) {
		t.Errorf("wallet balance: circle wallet amount_mloki = %v, want in (0, %d] (both redeems landed, maybe minus fees)", walletAmount, amount1+amount3+amount4)
	}
	total, _ := balanceResp["total_mloki"].(float64)
	if total != walletAmount {
		t.Errorf("wallet balance: total_mloki = %v, want exactly the single wallet line's amount %v", total, walletAmount)
	}

	// The local action log must show exactly this sequence, in order:
	// 4 receives, then redeem, transfer, consolidate, redeem.
	got := historyActions(t, u)
	want := []string{"receive", "receive", "receive", "receive", "redeem", "transfer", "consolidate", "redeem"}
	if len(got) != len(want) {
		t.Fatalf("wallet history: %d entries %v, want %d entries %v", len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wallet history[%d] = %q, want %q (full sequence: %v)", i, got[i], want[i], got)
			break
		}
	}
}

// TestSession_MultiWalletRedeemRouting joins two circle wallets, redeems
// different held tokens into each by name (not just the default), and
// confirms `wallet use` actually re-routes a subsequent destination-less
// redeem — the multi-wallet-per-identity case a client with more than one
// Lightning wallet registered (a personal one and a shared one, say)
// really hits.
func TestSession_MultiWalletRedeemRouting(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	u := newFixture(t)
	uInit := u.mustJSON("wallet", "init")
	uPub, err := npubToHex(uInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	circleHub1 := setUpCircleHub(t, admin, uPub)
	circleHub2 := setUpCircleHub(t, admin, uPub)
	if circleHub1.CircleHubToken == nil || circleHub2.CircleHubToken == nil {
		t.Fatalf("create ephemeral circle_hubs: missing circleHubToken(s): %+v / %+v", circleHub1, circleHub2)
	}
	join1 := u.mustJSON("join", "--hub", *circleHub1.CircleHubToken, "--max-amount", "100", "--yes")
	wallet1, _ := join1["wallet"].(string)
	if wallet1 == "" {
		t.Fatalf("join (1st): no wallet name in response: %v", join1)
	}
	if def, _ := join1["default"].(bool); !def {
		t.Errorf("join (1st, first-ever wallet): default = %v, want true", def)
	}
	join2 := u.mustJSON("join", "--hub", *circleHub2.CircleHubToken, "--max-amount", "100", "--yes")
	wallet2, _ := join2["wallet"].(string)
	if wallet2 == "" || wallet2 == wallet1 {
		t.Fatalf("join (2nd): wallet name = %q, want a distinct non-empty name from %q", wallet2, wallet1)
	}
	if def, _ := join2["default"].(bool); def {
		t.Errorf("join (2nd, --json mode, not first-ever): default = %v, want false (default stays on %q)", def, wallet1)
	}

	cashHub := setUpCashHub(t, admin)
	const amountA, amountB, amountC = uint64(12_000), uint64(18_000), uint64(9_000)
	tokenA := mintPubkeyTokenFromHub(t, cashHub, uPub, amountA)
	tokenB := mintPubkeyTokenFromHub(t, cashHub, uPub, amountB)
	receiveA := u.mustJSON("receive", tokenA)
	receiveB := u.mustJSON("receive", tokenB)
	idA, _ := receiveA["entry"].(map[string]any)["id"].(string)
	idB, _ := receiveB["entry"].(map[string]any)["id"].(string)

	// Into the default wallet (wallet1), no destination given.
	redeemA := u.mustJSON("redeem", "--token", idA, "--yes")
	if toWallet, _ := redeemA["to_wallet"].(string); toWallet != wallet1 {
		t.Errorf("redeem A (no destination): to_wallet = %q, want default %q", toWallet, wallet1)
	}
	// Explicitly into wallet2 by name.
	redeemB := u.mustJSON("redeem", "--token", idB, wallet2, "--yes")
	if toWallet, _ := redeemB["to_wallet"].(string); toWallet != wallet2 {
		t.Errorf("redeem B (explicit destination): to_wallet = %q, want %q", toWallet, wallet2)
	}

	balanceResp := u.mustJSON("wallet", "balance", "--breakdown")
	breakdown, _ := balanceResp["breakdown"].([]any)
	amounts := map[string]float64{}
	for _, line := range breakdown {
		l, _ := line.(map[string]any)
		if l["kind"] != "wallet" {
			continue
		}
		name, _ := l["name"].(string)
		amt, _ := l["amount_mloki"].(float64)
		amounts[name] = amt
	}
	if amounts[wallet1] <= 0 || amounts[wallet1] > float64(amountA) {
		t.Errorf("wallet balance: %q amount = %v, want in (0, %d]", wallet1, amounts[wallet1], amountA)
	}
	if amounts[wallet2] <= 0 || amounts[wallet2] > float64(amountB) {
		t.Errorf("wallet balance: %q amount = %v, want in (0, %d]", wallet2, amounts[wallet2], amountB)
	}

	// Switch the default to wallet2, then confirm a destination-less
	// redeem now routes there instead.
	if res := u.run("wallet", "use", wallet2, "--yes"); res.ExitCode != 0 {
		t.Fatalf("wallet use %s: exit %d\nstderr: %s", wallet2, res.ExitCode, res.Stderr)
	}
	tokenC := mintPubkeyTokenFromHub(t, cashHub, uPub, amountC)
	receiveC := u.mustJSON("receive", tokenC)
	idC, _ := receiveC["entry"].(map[string]any)["id"].(string)
	redeemC := u.mustJSON("redeem", "--token", idC, "--yes")
	if toWallet, _ := redeemC["to_wallet"].(string); toWallet != wallet2 {
		t.Errorf("redeem C (after `wallet use %s`, no destination): to_wallet = %q, want %q", wallet2, toWallet, wallet2)
	}
}
