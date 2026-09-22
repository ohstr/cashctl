//go:build integration

package integration

import (
	"testing"
)

// TestCashConsolidate_PartialSubsetLeavesRestUntouched holds 4 same-hub
// tokens and consolidates only 2 of them by explicit ID — the other 2 must
// remain individually held, at their original amounts, completely
// untouched. cash_test.go's own TestCashConsolidate always merges exactly
// the tokens it minted (2 of 2); this is the "tidy up just these, not
// everything" case a real client actually reaches for.
func TestCashConsolidate_PartialSubsetLeavesRestUntouched(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	const amount1, amount2, amount3, amount4 = uint64(10_000), uint64(20_000), uint64(30_000), uint64(40_000)
	token3 := mintPubkeyTokenFromHub(t, hub, bPub, amount3)
	token4 := mintPubkeyTokenFromHub(t, hub, bPub, amount4)
	var id1, id2 string
	for _, spec := range []struct {
		amount uint64
		idOut  *string
	}{
		{amount1, &id1},
		{amount2, &id2},
	} {
		tok := mintPubkeyTokenFromHub(t, hub, bPub, spec.amount)
		resp := b.mustJSON("receive", tok)
		entry, _ := resp["entry"].(map[string]any)
		*spec.idOut, _ = entry["id"].(string)
	}
	if res := b.run("receive", token3); res.ExitCode != 0 {
		t.Fatalf("receive (token3): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res := b.run("receive", token4); res.ExitCode != 0 {
		t.Fatalf("receive (token4): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	consolidateResp := b.mustJSON("consolidate", "--sources", id1+","+id2, "--yes")
	newEntry, _ := consolidateResp["new_entry"].(map[string]any)
	if amt, _ := newEntry["amount_millis"].(float64); uint64(amt) != amount1+amount2 {
		t.Fatalf("consolidate: new_entry.amount_millis = %v, want %d", newEntry["amount_millis"], amount1+amount2)
	}

	showResp := b.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 3 {
		t.Fatalf("expected 3 held tokens (1 merged + 2 untouched), got %d: %v", len(held), held)
	}
	wantUntouched := map[string]uint64{token3: amount3, token4: amount4}
	foundUntouched := map[string]bool{}
	for _, h := range held {
		e, _ := h.(map[string]any)
		tok, _ := e["token"].(string)
		if wantAmt, isUntouched := wantUntouched[tok]; isUntouched {
			foundUntouched[tok] = true
			if amt, _ := e["amount_millis"].(float64); uint64(amt) != wantAmt {
				t.Errorf("untouched token %q: amount_millis = %v, want %d (must be exactly as minted)", tok, e["amount_millis"], wantAmt)
			}
			if status, _ := e["status"].(string); status != "" && status != "held" {
				t.Errorf("untouched token %q: status = %q, want \"held\"", tok, status)
			}
		}
	}
	for tok := range wantUntouched {
		if !foundUntouched[tok] {
			t.Errorf("token %q (not part of the consolidate --sources call) is missing from held_tokens entirely: %v", tok, held)
		}
	}
}

// TestCashConsolidate_DirectToThirdParty merges two held tokens straight
// into a REAL second cashctl identity's own pubkey via `consolidate --to`
// — a merge-and-gift in one call, no separate transfer needed afterward.
// Verified by having that second identity actually `receive` the result,
// not just checking consolidate's own reported new_entry.
//
// Currently failing on purpose (left failing rather than skipped, same
// convention as TestMultiParty_CashRegiftChain_AtoBtoC's own doc
// comment): the cash_consolidate call itself fails with "nipcash: decrypt
// delivery: invalid MAC" — a NIP-44 response-decryption failure at the
// generic NIP-47 transport layer (nipcash/client.CashConsolidate is a
// thin wrapper: builds a request, calls the shared rawCall, parses the
// result — nothing consolidate-specific for cashctl's own code to get
// wrong). Every OTHER consolidate/transfer path this suite exercises to a
// real third-party pubkey goes through cash_transfer instead (this is the
// only test calling cash_consolidate --to a pubkey OTHER than the
// caller's own identity or cash) and succeeds, which narrows
// this specifically to cash_consolidate's own handling of a non-self
// pubkey target — most likely the Hub encrypting its response to the
// wrong key. Reproduced consistently across repeated runs (once also saw
// a plain timeout on the preceding mint_cash call, suggesting some
// instability around this exact path server-side); out of cashctl's own
// reach either way.
func TestCashConsolidate_DirectToThirdParty(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	const amount1, amount2 = uint64(20_000), uint64(30_000)
	token1 := mintPubkeyTokenFromHub(t, hub, bPub, amount1)
	token2 := mintPubkeyTokenFromHub(t, hub, bPub, amount2)
	receive1 := b.mustJSON("receive", token1)
	receive2 := b.mustJSON("receive", token2)
	id1, _ := receive1["entry"].(map[string]any)["id"].(string)
	id2, _ := receive2["entry"].(map[string]any)["id"].(string)
	if id1 == "" || id2 == "" {
		t.Fatalf("receive: missing entry id(s): %v / %v", receive1, receive2)
	}

	c := newFixture(t)
	cInit := c.mustJSON("wallet", "init")
	cPub, err := npubToHex(cInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode C's local identity npub: %v", err)
	}

	consolidateResp := b.mustJSON("consolidate", "--sources", id1+","+id2, "--to", cPub, "--yes")
	newEntry, _ := consolidateResp["new_entry"].(map[string]any)
	newToken, _ := newEntry["token"].(string)
	if newToken == "" {
		t.Fatalf("consolidate --to <C>: no new_entry.token in response: %v", consolidateResp)
	}
	if amt, _ := newEntry["amount_millis"].(float64); uint64(amt) != amount1+amount2 {
		t.Errorf("consolidate --to <C>: new_entry.amount_millis = %v, want %d", newEntry["amount_millis"], amount1+amount2)
	}

	if n := heldCount(t, b); n != 0 {
		t.Errorf("B: expected 0 held tokens after consolidating everything away to C, got %d", n)
	}

	cReceive := c.mustJSON("receive", newToken)
	cEntry, _ := cReceive["entry"].(map[string]any)
	if amt, _ := cEntry["amount_millis"].(float64); uint64(amt) != amount1+amount2 {
		t.Fatalf("C receive (consolidate --to gift): amount_millis = %v, want %d", cEntry["amount_millis"], amount1+amount2)
	}
}

// TestCashTransfer_CashSelection_MinimalSubsetSkipsExtraToken covers
// SelectForAmount's own greedy largest-first subset choice
// (smallestCoveringSubset) with a THIRD, smaller same-minter token that
// must be left out and left untouched — TestCashTransfer_CashSelection_AutoConsolidate
// only ever has exactly 2 candidates (both required), so it can't
// distinguish "consolidates everything same-minter" from "consolidates the
// minimal covering subset."
func TestCashTransfer_CashSelection_MinimalSubsetSkipsExtraToken(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	const (
		bigAmount    = uint64(50_000)
		midAmount    = uint64(30_000)
		smallAmount  = uint64(10_000)
		targetAmount = uint64(70_000) // covered by big+mid (80,000); small must be skipped
	)
	tokenBig, minterBig, okBig := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, bigAmount)
	if !okBig {
		t.Skip("skipping: this Hub did not attach a mint signature to the first token")
	}
	tokenMid, minterMid, okMid := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, midAmount)
	if !okMid {
		t.Skip("skipping: this Hub did not attach a mint signature to the second token")
	}
	tokenSmall, minterSmall, okSmall := mintSignedPubkeyTokenFromHub(t, b, hub, bPub, smallAmount)
	if !okSmall {
		t.Skip("skipping: this Hub did not attach a mint signature to the third token")
	}
	if minterBig != minterMid || minterMid != minterSmall {
		t.Skipf("skipping: recovered minter pubkeys differ (%s / %s / %s)", minterBig, minterMid, minterSmall)
	}
	for _, tok := range []string{tokenBig, tokenMid, tokenSmall} {
		if res := b.run("receive", tok); res.ExitCode != 0 {
			t.Fatalf("receive (%s): exit %d\nstderr: %s", tok, res.ExitCode, res.Stderr)
		}
	}

	c := newFixture(t)
	cInit := c.mustJSON("wallet", "init")
	cPub, err := npubToHex(cInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode C's local identity npub: %v", err)
	}

	transferResp := b.mustJSON("transfer", cPub, lokiArg(int64(targetAmount)), "--yes")
	consolidatedFrom, _ := transferResp["consolidated_from"].([]any)
	if len(consolidatedFrom) != 2 {
		t.Fatalf("transfer: consolidated_from = %v, want exactly 2 (big+mid, not the small token)", transferResp["consolidated_from"])
	}
	remaining, _ := transferResp["remaining_amount_millis"].(float64)
	if uint64(remaining) != bigAmount+midAmount-targetAmount {
		t.Fatalf("transfer: remaining_amount_millis = %v, want %d", transferResp["remaining_amount_millis"], bigAmount+midAmount-targetAmount)
	}

	// B must now hold exactly 2 tokens: the untouched small one (still its
	// own original token string) and the fresh remainder from the
	// consolidate-then-split, both worth 10,000 — a coincidence in amount
	// that makes the token-string check below the only way to actually
	// distinguish "skipped" from "swept in and split back out again."
	showB := b.mustJSON("wallet", "show")
	heldB, _ := showB["held_tokens"].([]any)
	if len(heldB) != 2 {
		t.Fatalf("B: expected 2 held tokens (untouched small + new remainder), got %d: %v", len(heldB), heldB)
	}
	foundOriginalSmall := false
	for _, h := range heldB {
		e, _ := h.(map[string]any)
		if tok, _ := e["token"].(string); tok == tokenSmall {
			foundOriginalSmall = true
			if amt, _ := e["amount_millis"].(float64); uint64(amt) != smallAmount {
				t.Errorf("the small token's amount changed: %v, want %d (must never be touched)", e["amount_millis"], smallAmount)
			}
		}
	}
	if !foundOriginalSmall {
		t.Errorf("the small, non-covering-needed token is no longer held under its ORIGINAL token string — it was swept into the consolidate despite not being needed: %v", heldB)
	}

	cReceive := c.mustJSON("receive", recipientTokenFromTransfer(transferResp, tokenBig))
	cEntry, _ := cReceive["entry"].(map[string]any)
	if amt, _ := cEntry["amount_millis"].(float64); uint64(amt) != targetAmount {
		t.Fatalf("C receive: amount_millis = %v, want %d", cEntry["amount_millis"], targetAmount)
	}
}

// TestCashTransfer_FragmentedThenManualSplitAcrossHubs is the realistic
// recovery path after cash selection refuses a fragmented payment: B holds
// enough money in aggregate, split across two different Cash Hubs, to
// cover what it owes C, but transfer correctly refuses to combine them
// silently. The real client's next move is two explicit, manual transfers
// (one per hub) reaching the same recipient — proven here by having C
// actually receive and sum both.
func TestCashTransfer_FragmentedThenManualSplitAcrossHubs(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	b := newFixture(t)
	bInit := b.mustJSON("wallet", "init")
	bPub, err := npubToHex(bInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub1 := setUpCashHub(t, admin)
	hub2 := setUpCashHub(t, admin)
	const amountH1, amountH2 = uint64(30_000), uint64(40_000)
	const owed = amountH1 + amountH2 - 10_000 // 60,000: needs both hubs, but hub2 only partially

	tokenH1 := mintPubkeyTokenFromHub(t, hub1, bPub, amountH1)
	tokenH2 := mintPubkeyTokenFromHub(t, hub2, bPub, amountH2)
	receiveH1 := b.mustJSON("receive", tokenH1)
	receiveH2 := b.mustJSON("receive", tokenH2)
	idH1, _ := receiveH1["entry"].(map[string]any)["id"].(string)
	idH2, _ := receiveH2["entry"].(map[string]any)["id"].(string)
	if idH1 == "" || idH2 == "" {
		t.Fatalf("receive: missing entry id(s): %v / %v", receiveH1, receiveH2)
	}

	c := newFixture(t)
	cInit := c.mustJSON("wallet", "init")
	cPub, err := npubToHex(cInit["npub"].(string))
	if err != nil {
		t.Fatalf("decode C's local identity npub: %v", err)
	}

	// The naive single call must refuse — the two hubs can't be combined
	// silently into one transfer.
	refused := b.run("transfer", cPub, lokiArg(int64(owed)), "--yes")
	if refused.ExitCode != 2 {
		t.Fatalf("transfer (fragmented across 2 hubs): exit = %d, want 2 (usage)\nstderr: %s", refused.ExitCode, refused.Stderr)
	}
	if !jsonErrorContains(t, refused.Stderr, "usage", "fragmented") {
		t.Errorf("unexpected error body: %s", refused.Stderr)
	}
	if n := heldCount(t, b); n != 2 {
		t.Fatalf("a refused transfer must leave both held tokens untouched, got %d", n)
	}

	// The manual workaround: send hub1's whole token, and split exactly
	// enough off hub2's to reach the owed total, keeping the rest.
	resp1 := b.mustJSON("transfer", cPub, "--token", idH1, "--yes")
	if remaining := resp1["remaining_amount_millis"]; remaining != nil {
		t.Errorf("manual transfer #1 (hub1, whole token): remaining_amount_millis = %v, want absent", remaining)
	}
	splitFromH2 := owed - amountH1
	resp2 := b.mustJSON("transfer", cPub, lokiArg(int64(splitFromH2)), "--token", idH2, "--yes")
	remaining2, _ := resp2["remaining_amount_millis"].(float64)
	if uint64(remaining2) != amountH2-splitFromH2 {
		t.Fatalf("manual transfer #2 (hub2, split): remaining_amount_millis = %v, want %d", resp2["remaining_amount_millis"], amountH2-splitFromH2)
	}

	if n := heldCount(t, b); n != 1 {
		t.Fatalf("B: expected 1 held token (hub2's remainder) after the manual two-part send, got %d", n)
	}

	c.mustJSON("receive", recipientTokenFromTransfer(resp1, tokenH1))
	c.mustJSON("receive", recipientTokenFromTransfer(resp2, tokenH2))
	showC := c.mustJSON("wallet", "show")
	heldC, _ := showC["held_tokens"].([]any)
	if len(heldC) != 2 {
		t.Fatalf("C: expected 2 held tokens (one per manual transfer), got %d: %v", len(heldC), heldC)
	}
	var totalC uint64
	for _, h := range heldC {
		e, _ := h.(map[string]any)
		if amt, _ := e["amount_millis"].(float64); amt > 0 {
			totalC += uint64(amt)
		}
	}
	if totalC != owed {
		t.Errorf("C: total received across both manual transfers = %d, want %d", totalC, owed)
	}
}
