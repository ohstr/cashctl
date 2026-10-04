//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// TestCashReceive_AutoSecuresCashReceipt_NoOtherHoldings is scenario (a)
// from docs/private/auto-secure-bearer-receipt-plan.md's Verification
// section: receiving a cash gift with no other same-minter holdings
// re-keys it in place (Confirm auto-accepts under --json, same as every
// other confirm in this codebase) — the original secret must be dead
// afterward, and the ledger's own updated secret must still redeem for
// real.
func TestCashReceive_AutoSecuresCashReceipt_NoOtherHoldings(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	hub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)

	const amountMillis = uint64(60_000)
	mintResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), amountMillis)},
	})
	if err != nil {
		t.Fatalf("mint_cash (cash): %v", err)
	}
	if len(mintResult.Recipients) != 1 || mintResult.Recipients[0].CashSecret == "" {
		t.Fatalf("mint_cash (cash): expected exactly one recipient with a cash_secret: %+v", mintResult.Recipients)
	}
	originalSecret := mintResult.Recipients[0].CashSecret
	giftString := mintResult.CashToken + "#" + originalSecret

	receiveResp := f.mustJSON("receive", giftString)
	secured, _ := receiveResp["secured"].(map[string]any)
	if secured == nil {
		t.Fatalf("receive: no \"secured\" report in response: %v", receiveResp)
	}
	if secured["status"] != "rekeyed" {
		t.Fatalf(`receive: secured.status = %v, want "rekeyed" (no other same-minter holdings, so this should re-key in place, not consolidate)`, secured["status"])
	}

	// The original secret must now be dead — a direct attempt to spend it
	// against the Hub must fail.
	origClient, err := nipcashclient.Connect(ctx, mintResult.CashToken)
	if err != nil {
		t.Fatalf("dial original token: %v", err)
	}
	defer origClient.Close()
	if _, err := origClient.CashTransfer(ctx, nipcash.CashTransferParams{
		Credential:    nipcash.BySecret(originalSecret),
		To:            nipcash.NewCashTarget(),
		CurrentAmount: amountMillis,
	}); err == nil {
		t.Error("original cash secret still spends after receive auto-secured it — it should have been re-keyed dead")
	}

	// The ledger's own updated secret (never re-shown to this test — it's
	// applied straight to the held entry) must still be a genuine,
	// spendable credential.
	hubClient := dialNWC(t, ctx, hub.PairingUri)
	invoiceTx, err := hubClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}
	redeemResp := f.mustJSON("redeem", "--invoice", invoiceTx.Invoice, "--yes")
	if preimage, _ := redeemResp["preimage"].(string); preimage == "" {
		t.Errorf("redeem (after auto-secure): empty preimage — the re-keyed secret wasn't a working credential: %v", redeemResp)
	}
}

// TestWalletProtect_ManuallyProtectsADeclinedCashReceipt is the live
// evidence for the fix to "a single unprotected cash-mode holding can never
// be re-protected later": the documented recovery,
// `consolidate --to cash`, needs 2+ sources and can't re-key one
// holding alone — `wallet protect` (cmd/wallet_protect.go) reuses the
// exact same protectRekeyOnly logic `receive`'s own automatic offer uses,
// just triggered manually for a holding that missed it. Also proves
// Entry.CashProtection tracks the state correctly end to end: "shared"
// right after a declined receive, "protected" after a successful manual
// protect.
func TestWalletProtect_ManuallyProtectsADeclinedCashReceipt(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	f := newFixture(t)
	f.mustJSON("wallet", "init")

	hub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)

	const amountMillis = uint64(45_000)
	mintResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), amountMillis)},
	})
	if err != nil {
		t.Fatalf("mint_cash (cash): %v", err)
	}
	if len(mintResult.Recipients) != 1 || mintResult.Recipients[0].CashSecret == "" {
		t.Fatalf("mint_cash (cash): expected exactly one recipient with a cash_secret: %+v", mintResult.Recipients)
	}
	originalSecret := mintResult.Recipients[0].CashSecret
	giftString := mintResult.CashToken + "#" + originalSecret

	// Decline the automatic protect offer ("n", not a bare Enter — the
	// prompt's own default is yes; this test is specifically about the
	// holding that DIDN'T get protected at receive time).
	res := f.runInteractive("n\n", "receive", giftString)
	if res.ExitCode != 0 {
		t.Fatalf("receive (decline protect): exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("wallet show: got %d held tokens, want 1: %v", len(held), held)
	}
	entry, _ := held[0].(map[string]any)
	if got, _ := entry["cash_protection"].(string); got != "shared" {
		t.Fatalf(`wallet show: cash_protection = %q, want "shared" (protect was declined)`, got)
	}
	entryID, _ := entry["id"].(string)
	if entryID == "" {
		t.Fatalf("wallet show: held entry has no id: %v", entry)
	}

	// The manual protect — no --token needed, exactly one eligible holding.
	protectResp := f.mustJSON("wallet", "protect")
	protected, _ := protectResp["protected"].(map[string]any)
	if protected == nil || protected["status"] != "rekeyed" {
		t.Fatalf(`wallet protect: protected.status = %v, want "rekeyed": %v`, protected["status"], protectResp)
	}

	// The original secret must now be dead.
	origClient, err := nipcashclient.Connect(ctx, mintResult.CashToken)
	if err != nil {
		t.Fatalf("dial original token: %v", err)
	}
	defer origClient.Close()
	if _, err := origClient.CashTransfer(ctx, nipcash.CashTransferParams{
		Credential:    nipcash.BySecret(originalSecret),
		To:            nipcash.NewCashTarget(),
		CurrentAmount: amountMillis,
	}); err == nil {
		t.Error("original cash secret still spends after wallet protect — it should have been re-keyed dead")
	}

	// wallet show must now report it as protected, same entry ID.
	afterResp := f.mustJSON("wallet", "show")
	afterHeld, _ := afterResp["held_tokens"].([]any)
	if len(afterHeld) != 1 {
		t.Fatalf("wallet show (after protect): got %d held tokens, want 1: %v", len(afterHeld), afterHeld)
	}
	afterEntry, _ := afterHeld[0].(map[string]any)
	if got, _ := afterEntry["cash_protection"].(string); got != "protected" {
		t.Errorf(`wallet show (after protect): cash_protection = %q, want "protected"`, got)
	}
	if got, _ := afterEntry["id"].(string); got != entryID {
		t.Errorf("wallet show (after protect): id changed from %q to %q — protectRekeyOnly re-keys in place, never a new entry", entryID, got)
	}

	// The re-keyed secret must still be a genuine, spendable credential.
	hubClient := dialNWC(t, ctx, hub.PairingUri)
	invoiceTx, err := hubClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}
	redeemResp := f.mustJSON("redeem", "--invoice", invoiceTx.Invoice, "--yes")
	if preimage, _ := redeemResp["preimage"].(string); preimage == "" {
		t.Errorf("redeem (after manual protect): empty preimage — the re-keyed secret wasn't a working credential: %v", redeemResp)
	}
}

// TestWalletProtect_NothingToProtectIsNotFound confirms the empty case: a
// wallet with no unprotected cash-mode holdings gets a clear, specific
// answer, not a generic error.
func TestWalletProtect_NothingToProtectIsNotFound(t *testing.T) {
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	res := f.run("wallet", "protect")
	if res.ExitCode != 4 {
		t.Fatalf("wallet protect (nothing held): exit %d, want 4 (not_found)\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	if !jsonErrorContains(t, res.Stderr, "not_found", "nothing to protect") {
		t.Errorf("unexpected error body: %s", res.Stderr)
	}
}

// TestCashReceive_AutoSecuresCashReceipt_MergesWithExistingHolding is
// scenario (b) from the same plan: receiving a cash gift while already
// holding another mint-signed token from the same minter merges them into
// one fresh cash note — both original sources must independently show
// fully claimed on the Hub (not just trusted from cashctl's own local
// bookkeeping), and the merged amount must actually redeem.
func TestCashReceive_AutoSecuresCashReceipt_MergesWithExistingHolding(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)

	const (
		existingAmount = uint64(30_000)
		cashAmount     = uint64(50_000)
	)

	// An existing pubkey-mode holding, mint-signed, from the same Hub —
	// same helper (and same signature-best-effort skip) as
	// TestCashTransfer_CashSelection_AutoConsolidate.
	existingToken, minter1, ok1 := mintSignedPubkeyTokenFromHub(t, f, hub, myPubHex, existingAmount)
	if !ok1 {
		t.Skip("skipping: this Hub did not attach a mint signature (best-effort — see NIP-CASH §Mint Provenance) — same-minter grouping can't be exercised without one")
	}
	if res := f.run("receive", existingToken); res.ExitCode != 0 {
		t.Fatalf("receive (existing pubkey-mode holding): exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	cashResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), cashAmount)},
	})
	if err != nil {
		t.Fatalf("mint_cash (cash, signed): %v", err)
	}
	if len(cashResult.Recipients) != 1 || cashResult.Recipients[0].CashSecret == "" {
		t.Fatalf("mint_cash (cash, signed): expected exactly one recipient with a cash_secret: %+v", cashResult.Recipients)
	}
	decodeResp := f.mustJSON("decode", cashResult.CashToken)
	minter2, valid := decodeResp["minter_pubkey"].(string)
	if !valid || minter2 == "" {
		t.Skip("skipping: this Hub did not attach a mint signature to the cash-mode mint")
	}
	if minter1 != minter2 {
		t.Skipf("skipping: the two tokens' recovered minter pubkeys differ (%s vs %s) — can't exercise same-minter grouping", minter1, minter2)
	}

	giftString := cashResult.CashToken + "#" + cashResult.Recipients[0].CashSecret
	receiveResp := f.mustJSON("receive", giftString)
	secured, _ := receiveResp["secured"].(map[string]any)
	if secured == nil {
		t.Fatalf("receive: no \"secured\" report in response: %v", receiveResp)
	}
	if secured["status"] != "consolidated" {
		t.Fatalf(`receive: secured.status = %v, want "consolidated" (an existing same-minter holding should trigger a merge); secured.error = %v`, secured["status"], secured["error"])
	}
	consolidatedWith, _ := secured["consolidated_with"].([]any)
	if len(consolidatedWith) != 1 {
		t.Fatalf("secured.consolidated_with = %v, want exactly 1 entry", secured["consolidated_with"])
	}

	entry, _ := receiveResp["entry"].(map[string]any)
	gotAmount, _ := entry["amount_millis"].(float64)
	if uint64(gotAmount) != existingAmount+cashAmount {
		t.Errorf("final entry amount_millis = %v, want %d", entry["amount_millis"], existingAmount+cashAmount)
	}

	// Independently verify server-side: both original sources were merged
	// away, so neither bill exists any more.
	for _, tok := range []string{existingToken, cashResult.CashToken} {
		requireBillSpentAway(t, admin, hub.ID, tok, "merged source")
	}

	hubClient := dialNWC(t, ctx, hub.PairingUri)
	invoiceTx, err := hubClient.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(existingAmount + cashAmount)})
	if err != nil {
		t.Fatalf("make_invoice: %v", err)
	}
	redeemResp := f.mustJSON("redeem", "--invoice", invoiceTx.Invoice, "--yes")
	if preimage, _ := redeemResp["preimage"].(string); preimage == "" {
		t.Errorf("redeem (merged holding): empty preimage: %v", redeemResp)
	}
}

// TestCashReceive_AutoSecuresCashReceipt_ExcludesExpiredSiblingFromMerge is
// TestCashReceive_AutoSecuresCashReceipt_MergesWithExistingHolding's
// counterpart once the existing same-minter holding is already expired by
// the time the fresh gift arrives: buildConsolidateWith now excludes a
// confirmed-expired sibling from the merge rather than poisoning the
// fresh receipt's own good deadline with it (NIP-CASH's merge rule
// inherits the earliest expiry across every source). The only other
// candidate is excluded, so the merge set is empty and protectCashReceipt
// falls back to its no-merge rekey-only path — the fresh receipt still
// ends up protected, just not merged, and the expired sibling is left
// exactly as it was.
func TestCashReceive_AutoSecuresCashReceipt_ExcludesExpiredSiblingFromMerge(t *testing.T) {
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
	npub, _ := initResp["npub"].(string)
	myPubHex, err := npubToHex(npub)
	if err != nil {
		t.Fatalf("decode local identity npub: %v", err)
	}

	hub := setUpCashHub(t, admin) // default 1h ceiling — plenty for the fresh gift
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)

	const (
		existingAmount = uint64(20_000)
		cashAmount     = uint64(40_000)
	)

	// The existing holding, mint-signed, deliberately short-lived — same
	// helper cash_expiration_matrix_test.go's own expiry tests use.
	existingToken, minter1, ok1 := mintSignedPubkeyTokenExpiry(t, f, hub, myPubHex, existingAmount, shortCashExpirySecs)
	if !ok1 {
		t.Skip("skipping: this Hub did not attach a mint signature (best-effort — see NIP-CASH §Mint Provenance) — same-minter grouping can't be exercised without one")
	}
	receiveExisting := f.mustJSON("receive", existingToken)
	existingID, _ := receiveExisting["entry"].(map[string]any)["id"].(string)
	if existingID == "" {
		t.Fatalf("receive (existing holding): missing entry id: %v", receiveExisting)
	}

	waitPastCashExpiry()

	// Minted AFTER the wait: the fresh gift itself must stay healthy, only
	// the existing sibling is expired.
	cashResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients: []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), cashAmount)},
	})
	if err != nil {
		t.Fatalf("mint_cash (cash, signed): %v", err)
	}
	if len(cashResult.Recipients) != 1 || cashResult.Recipients[0].CashSecret == "" {
		t.Fatalf("mint_cash (cash, signed): expected exactly one recipient with a cash_secret: %+v", cashResult.Recipients)
	}
	decodeResp := f.mustJSON("decode", cashResult.CashToken)
	minter2, valid := decodeResp["minter_pubkey"].(string)
	if !valid || minter2 == "" {
		t.Skip("skipping: this Hub did not attach a mint signature to the cash-mode mint")
	}
	if minter1 != minter2 {
		t.Skipf("skipping: the two tokens' recovered minter pubkeys differ (%s vs %s) — can't exercise same-minter grouping", minter1, minter2)
	}

	giftString := cashResult.CashToken + "#" + cashResult.Recipients[0].CashSecret
	receiveResp := f.mustJSON("receive", giftString)
	secured, _ := receiveResp["secured"].(map[string]any)
	if secured == nil {
		t.Fatalf("receive: no \"secured\" report in response: %v", receiveResp)
	}
	// Not "consolidated": the only other same-minter candidate is already
	// expired and must be excluded, leaving nothing to merge with.
	if secured["status"] != "rekeyed" {
		t.Fatalf(`receive: secured.status = %v, want "rekeyed" (the only same-minter sibling is expired and must be excluded, not merged); secured.error = %v`, secured["status"], secured["error"])
	}

	entry, _ := receiveResp["entry"].(map[string]any)
	gotAmount, _ := entry["amount_millis"].(float64)
	if uint64(gotAmount) != cashAmount {
		t.Errorf("final entry amount_millis = %v, want %d (rekeyed alone, not merged with the expired sibling)", entry["amount_millis"], cashAmount)
	}

	// The expired sibling is left exactly as it was: still held, under its
	// own ID, same amount — never touched by the merge it was excluded
	// from.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	if len(held) != 2 {
		t.Fatalf("wallet show: held_tokens = %v, want exactly 2 (expired sibling untouched, fresh receipt rekeyed separately)", held)
	}
	var foundExisting bool
	for _, h := range held {
		row, _ := h.(map[string]any)
		if row["id"] == existingID {
			foundExisting = true
			if got, _ := row["amount_millis"].(float64); uint64(got) != existingAmount {
				t.Errorf("existing (expired) entry amount_millis = %v, want unchanged %d", row["amount_millis"], existingAmount)
			}
		}
	}
	if !foundExisting {
		t.Errorf("existing (expired) entry %s no longer held — must not be consumed by the excluded merge", existingID)
	}
}
