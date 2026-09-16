//go:build integration

// This file exercises GENUINE concurrency from cashctl's own CLI: real
// compiled `cashctl` subprocesses launched simultaneously (synchronized
// with a barrier, not just fired in quick succession — see
// runConcurrently) against the SAME --config-dir/cashctl.db. lokihub's own
// audit suite already stress-tests the SERVER side of exactly this shape of
// race (see lokihub/integration/{audit_dynamic_test,
// cash_transfer_audit_replay_race_test, cash_transfer_audit_split_race_test,
// cash_consolidate_adversarial_test}.go) — nobody had tried the CLIENT's own
// local ledger (internal/ledger/ledger.go) under real concurrent writers
// before this file. It caught a real lost-update bug in Ledger.Save's old
// "wipe the whole table, reinsert everything I currently have in memory"
// shape: two concurrent cashctl processes against the same --config-dir
// each Load their own full snapshot, and a redeem/transfer/consolidate's
// own wire round trip easily outlasts another process's entire
// Load-mutate-Save cycle — whichever process's Save committed last silently
// reverted the other's status change back to "held" (a spent/consolidated/
// transferred token resurrected in wallet show) and erased its history
// line. Fixed by making Save diff against what Load actually saw and only
// touch rows this process itself changed — see internal/ledger/ledger.go's
// own doc comments on Ledger/Save, and its ledger_test.go's
// TestSave_ConcurrentDisjointWritesDoNotClobber for the fast, deterministic
// (no real subprocesses/network needed) version of the same regression.
// Full writeup: docs/private/audit-round2-race-adversarial.md.
//
// Round 3 (docs/private/audit-round3-race-adversarial-followup.md) adds
// TestRace_ConcurrentReceiveSameToken (two processes racing to receive the
// IDENTICAL token under the same --config-dir — does round 2's own
// upsert-by-ID Save make a silent local double-count possible?) and
// TestRace_ConcurrentJoinDifferentConfigDirs (two genuinely separate
// --config-dirs joining the same circle_hub at the same instant — a
// sanity check that cashctl's own local bookkeeping has no shared-state
// assumption that breaks, since each is really a separate "user").
package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// runConcurrently launches len(calls) real cashctl subprocesses against f,
// synchronized with a barrier so they fire as close to simultaneously as OS
// scheduling allows — every goroutine signals "ready" (past its own setup,
// about to exec) before any of them is released, so the race window isn't
// skewed by one goroutine getting a head start over another. Returns each
// invocation's result in the same order as calls.
func runConcurrently(f *fixture, calls ...[]string) []result {
	results := make([]result, len(calls))
	var wg, ready sync.WaitGroup
	start := make(chan struct{})
	wg.Add(len(calls))
	ready.Add(len(calls))
	for i, args := range calls {
		i, args := i, args
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			results[i] = f.run(args...)
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	return results
}

// fixtureCall pairs a fixture with the args to run against it — for
// runConcurrentAcrossFixtures, where (unlike runConcurrently) each
// subprocess targets a DIFFERENT --config-dir/identity, not a shared one.
type fixtureCall struct {
	f    *fixture
	args []string
}

// runConcurrentAcrossFixtures is runConcurrently's counterpart for two or
// more genuinely separate cashctl "users" (distinct fixtures, distinct
// --config-dirs) acting at the same instant against a shared remote
// resource (e.g. the same circle_hub) — same barrier-release shape, just
// keyed by fixture instead of assuming one shared f.
func runConcurrentAcrossFixtures(calls ...fixtureCall) []result {
	results := make([]result, len(calls))
	var wg, ready sync.WaitGroup
	start := make(chan struct{})
	wg.Add(len(calls))
	ready.Add(len(calls))
	for i, c := range calls {
		i, c := i, c
		go func() {
			defer wg.Done()
			ready.Done()
			<-start
			results[i] = c.f.run(c.args...)
		}()
	}
	ready.Wait()
	close(start)
	wg.Wait()
	return results
}

// decodeJSON decodes res's stdout as JSON — mustJSON's own decode half,
// usable on a result already collected from a race goroutine. mustJSON
// itself calls t.Fatalf, which is unsafe to call from any goroutine but the
// test's own (testing.T.FailNow's contract), so a race goroutine must use
// the plain f.run and have its result decoded back on the main goroutine.
func decodeJSON(t *testing.T, res result) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &out); err != nil {
		t.Fatalf("decode JSON stdout: %v\nstdout: %s\nstderr: %s", err, res.Stdout, res.Stderr)
	}
	return out
}

// heldTokenSet turns a wallet-show response's held_tokens into a set of
// original token strings, for cheap membership checks below.
func heldTokenSet(t *testing.T, showResp map[string]any) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	held, _ := showResp["held_tokens"].([]any)
	for _, h := range held {
		e, _ := h.(map[string]any)
		if tok, _ := e["token"].(string); tok != "" {
			set[tok] = true
		}
	}
	return set
}

// historyActionCounts turns a wallet-history response into a per-action
// tally, for cheap "was this recorded at all" checks below.
func historyActionCounts(t *testing.T, historyResp map[string]any) map[string]int {
	t.Helper()
	counts := map[string]int{}
	history, _ := historyResp["history"].([]any)
	for _, h := range history {
		e, _ := h.(map[string]any)
		if action, _ := e["action"].(string); action != "" {
			counts[action]++
		}
	}
	return counts
}

// TestRace_ConcurrentRedeemSameToken fires two `cashctl redeem` attempts
// against the exact same held local entry (same --config-dir, same
// underlying cash slice) at the same instant. Exactly one must actually
// redeem the money — lokihub's own audit already proves the SERVER never
// double-pays here; what this test proves is that cashctl's LOCAL ledger
// comes out clean regardless of which of the two processes the server
// happened to let win: no duplicate "redeem" history line, no resurrected
// held token. A nonzero RedeemFeePpm widens this beyond a fee-free race —
// the winning process's own fee bookkeeping (FeesPaid) is exercised under
// the exact same concurrent pressure.
func TestRace_ConcurrentRedeemSameToken(t *testing.T) {
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

	const amountMillis = uint64(200_000)
	hub := setUpCashHubOpts(t, admin, cashHubOpts{RedeemFeePpm: 20_000}) // 2%
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)
	receiveResp := f.mustJSON("receive", token)
	tokenID, _ := receiveResp["entry"].(map[string]any)["id"].(string)
	if tokenID == "" {
		t.Fatalf("receive: no entry id in response: %v", receiveResp)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	hubNWC := dialNWC(t, ctx, hub.PairingUri)
	invoice1, err := hubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice (1): %v", err)
	}
	invoice2, err := hubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountMillis)})
	if err != nil {
		t.Fatalf("make_invoice (2): %v", err)
	}

	results := runConcurrently(f,
		[]string{"redeem", "--token", tokenID, "--invoice", invoice1.Invoice, "--yes"},
		[]string{"redeem", "--token", tokenID, "--invoice", invoice2.Invoice, "--yes"},
	)

	successes := 0
	for _, r := range results {
		if r.ExitCode == 0 {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent redeem of the same token: %d of 2 succeeded, want exactly 1 (results: %+v)", successes, results)
	}

	showResp := f.mustJSON("wallet", "show")
	if held := heldTokenSet(t, showResp); held[token] {
		t.Errorf("a concurrently-redeemed token must not remain held: %v", showResp["held_tokens"])
	}
	historyResp := f.mustJSON("wallet", "history")
	if counts := historyActionCounts(t, historyResp); counts["redeem"] != 1 {
		t.Errorf(`wallet history has %d "redeem" entries after a concurrent race, want exactly 1: %v`, counts["redeem"], historyResp["history"])
	}

	// Ground truth, not cashctl's own self-report.
	verifyClient, err := nipcashclient.Connect(ctx, token)
	if err != nil {
		t.Fatalf("dial original token connection: %v", err)
	}
	defer verifyClient.Close()
	recipients, err := verifyClient.ListRecipients(ctx)
	if err != nil {
		t.Fatalf("list_recipients: %v", err)
	}
	claimed := 0
	for _, r := range recipients.Recipients {
		if r.Claimed {
			claimed++
		}
	}
	if claimed != 1 {
		t.Errorf("server-side: %d recipients claimed, want exactly 1: %+v", claimed, recipients.Recipients)
	}
}

// TestRace_TransferConsolidateDisjointTokens_LostUpdate is this file's core
// scenario: a `transfer` of held token A races a `consolidate` of held
// tokens B+C — all three DISJOINT, all under the same --config-dir, so
// there's no server-side reason either call should fail. This is exactly
// the shape that used to lose data locally: both processes Load the same
// three-token snapshot, each does its own real (successful) wire round
// trip, and whichever Save committed last used to blindly overwrite the
// whole entries/history tables from its own now-stale in-memory copy —
// resurrecting A (or B/C) as "held" again despite being genuinely spent,
// dropping the OTHER process's history line entirely, and in the worst
// direction (transfer's Save landing last), erasing all local record of the
// newly consolidated token even though it's real, spendable money sitting
// under the caller's own identity server-side.
func TestRace_TransferConsolidateDisjointTokens_LostUpdate(t *testing.T) {
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

	const amountA, amountB, amountC = uint64(50_000), uint64(30_000), uint64(20_000)
	hub := setUpCashHub(t, admin)
	tokenA := mintPubkeyTokenFromHub(t, hub, myPubHex, amountA)
	tokenB := mintPubkeyTokenFromHub(t, hub, myPubHex, amountB)
	tokenC := mintPubkeyTokenFromHub(t, hub, myPubHex, amountC)

	receiveA := f.mustJSON("receive", tokenA)
	receiveB := f.mustJSON("receive", tokenB)
	receiveC := f.mustJSON("receive", tokenC)
	idA, _ := receiveA["entry"].(map[string]any)["id"].(string)
	idB, _ := receiveB["entry"].(map[string]any)["id"].(string)
	idC, _ := receiveC["entry"].(map[string]any)["id"].(string)
	if idA == "" || idB == "" || idC == "" {
		t.Fatalf("receive: missing entry id(s): A=%v B=%v C=%v", receiveA, receiveB, receiveC)
	}

	targetHex := fakeHex32(t)

	results := runConcurrently(f,
		[]string{"transfer", "--token", idA, "--to", "pubkey:" + targetHex, "--yes"},
		[]string{"consolidate", "--sources", idB + "," + idC, "--yes"},
	)
	transferRes, consolidateRes := results[0], results[1]
	if transferRes.ExitCode != 0 {
		t.Fatalf("concurrent transfer(A): exit %d\nstdout: %s\nstderr: %s", transferRes.ExitCode, transferRes.Stdout, transferRes.Stderr)
	}
	if consolidateRes.ExitCode != 0 {
		t.Fatalf("concurrent consolidate(B,C): exit %d\nstdout: %s\nstderr: %s", consolidateRes.ExitCode, consolidateRes.Stdout, consolidateRes.Stderr)
	}
	consolidateResp := decodeJSON(t, consolidateRes)
	newEntry, _ := consolidateResp["new_entry"].(map[string]any)
	newToken, _ := newEntry["token"].(string)
	if newToken == "" {
		t.Fatalf("consolidate: no new_entry.token in response: %v", consolidateResp)
	}

	// Ground truth first, independent of either command's own self-report.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	origAClient, err := nipcashclient.Connect(ctx, tokenA)
	if err != nil {
		t.Fatalf("dial original A connection: %v", err)
	}
	defer origAClient.Close()
	aRecipients, err := origAClient.ListRecipients(ctx)
	if err != nil {
		t.Fatalf("list_recipients (A): %v", err)
	}
	foundTarget := false
	for _, r := range aRecipients.Recipients {
		if r.IdentityType == "pubkey" && r.IdentityValue == targetHex {
			foundTarget = true
			if r.AmountMillis != amountA {
				t.Errorf("server: A reassigned to target with amount %d, want %d", r.AmountMillis, amountA)
			}
		}
	}
	if !foundTarget {
		t.Errorf("server: A's connection doesn't show the reassignment to target: %+v", aRecipients.Recipients)
	}

	newClient, err := nipcashclient.Connect(ctx, newToken)
	if err != nil {
		t.Fatalf("dial new consolidated token: %v", err)
	}
	defer newClient.Close()
	newRecipients, err := newClient.ListRecipients(ctx)
	if err != nil {
		t.Fatalf("list_recipients (new consolidated token): %v", err)
	}
	var newTotal uint64
	for _, r := range newRecipients.Recipients {
		newTotal += r.AmountMillis
	}
	if newTotal != amountB+amountC {
		t.Errorf("server: consolidated token totals %d, want %d", newTotal, amountB+amountC)
	}

	// Now the local ledger — this is what the race actually threatens.
	showResp := f.mustJSON("wallet", "show")
	held := heldTokenSet(t, showResp)
	if held[tokenA] {
		t.Errorf("BUG: token A reappears as held after being genuinely transferred away — a resurrected held token (lost update in Ledger.Save): held=%v", showResp["held_tokens"])
	}
	if held[tokenB] {
		t.Errorf("BUG: token B reappears as held after being genuinely consolidated away: held=%v", showResp["held_tokens"])
	}
	if held[tokenC] {
		t.Errorf("BUG: token C reappears as held after being genuinely consolidated away: held=%v", showResp["held_tokens"])
	}
	if !held[newToken] {
		t.Errorf("BUG: the newly consolidated token is missing from wallet show entirely — real, spendable money with no local record: held=%v", showResp["held_tokens"])
	}

	historyResp := f.mustJSON("wallet", "history")
	counts := historyActionCounts(t, historyResp)
	if counts["transfer"] == 0 {
		t.Errorf(`BUG: wallet history has no "transfer" entry — process A's history line was erased by process B's later Save: %v`, historyResp["history"])
	}
	if counts["consolidate"] == 0 {
		t.Errorf(`BUG: wallet history has no "consolidate" entry: %v`, historyResp["history"])
	}
}

// TestRace_RedeemUnderFeeAndExpiryPressureConcurrentWithConsolidate repeats
// this file's core disjoint-write race (see
// TestRace_TransferConsolidateDisjointTokens_LostUpdate) but widens the
// parameter space per this round's brief: a nonzero RedeemFeePpm (changes
// the amounts/bookkeeping on the redeem side) and a short MaxExpSecs fired
// with no deliberate delay, so D's own redeem genuinely races its Hub's
// expiry window closing rather than deterministically landing on one side
// of it — best-effort by nature (the exact outcome of that inner race isn't
// controlled from a black-box CLI test), so both outcomes are asserted:
// either way, the concurrent consolidate(E,F)'s write must survive intact,
// and D itself must never end up in an inconsistent state (resurrected if
// redeemed, silently dropped if the redeem was cleanly refused).
func TestRace_RedeemUnderFeeAndExpiryPressureConcurrentWithConsolidate(t *testing.T) {
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

	const amountD, amountE, amountF = uint64(40_000), uint64(15_000), uint64(25_000)
	const shortExpSecs = 4
	shortHub := setUpCashHubOpts(t, admin, cashHubOpts{MaxExpSecs: shortExpSecs, RedeemFeePpm: 30_000}) // 3% fee
	normalHub := setUpCashHub(t, admin)

	tokenD := mintPubkeyTokenFromHub(t, shortHub, myPubHex, amountD)
	tokenE := mintPubkeyTokenFromHub(t, normalHub, myPubHex, amountE)
	tokenF := mintPubkeyTokenFromHub(t, normalHub, myPubHex, amountF)

	receiveD := f.mustJSON("receive", tokenD)
	receiveE := f.mustJSON("receive", tokenE)
	receiveF := f.mustJSON("receive", tokenF)
	idD, _ := receiveD["entry"].(map[string]any)["id"].(string)
	idE, _ := receiveE["entry"].(map[string]any)["id"].(string)
	idF, _ := receiveF["entry"].(map[string]any)["id"].(string)
	if idD == "" || idE == "" || idF == "" {
		t.Fatalf("receive: missing entry id(s): D=%v E=%v F=%v", receiveD, receiveE, receiveF)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shortHubNWC := dialNWC(t, ctx, shortHub.PairingUri)
	invoiceD, err := shortHubNWC.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: int64(amountD)})
	if err != nil {
		t.Fatalf("make_invoice (D): %v", err)
	}

	results := runConcurrently(f,
		[]string{"redeem", "--token", idD, "--invoice", invoiceD.Invoice, "--yes"},
		[]string{"consolidate", "--sources", idE + "," + idF, "--yes"},
	)
	redeemRes, consolidateRes := results[0], results[1]

	if consolidateRes.ExitCode != 0 {
		t.Fatalf("concurrent consolidate(E,F) under fee/expiry pressure: exit %d\nstdout: %s\nstderr: %s", consolidateRes.ExitCode, consolidateRes.Stdout, consolidateRes.Stderr)
	}
	consolidateResp := decodeJSON(t, consolidateRes)
	newEntry, _ := consolidateResp["new_entry"].(map[string]any)
	newToken, _ := newEntry["token"].(string)
	if newToken == "" {
		t.Fatalf("consolidate: no new_entry.token in response: %v", consolidateResp)
	}

	showResp := f.mustJSON("wallet", "show")
	held := heldTokenSet(t, showResp)
	if held[tokenE] {
		t.Errorf("BUG: token E reappears as held after being consolidated away (fee/expiry-pressure race): held=%v", showResp["held_tokens"])
	}
	if held[tokenF] {
		t.Errorf("BUG: token F reappears as held after being consolidated away: held=%v", showResp["held_tokens"])
	}
	if !held[newToken] {
		t.Errorf("BUG: the newly consolidated token is missing entirely from wallet show: held=%v", showResp["held_tokens"])
	}

	historyResp := f.mustJSON("wallet", "history")
	counts := historyActionCounts(t, historyResp)
	if counts["consolidate"] == 0 {
		t.Errorf(`BUG: wallet history has no "consolidate" entry: %v`, historyResp["history"])
	}

	if redeemRes.ExitCode == 0 {
		t.Logf("redeem(D) won its race against the %ds expiry window", shortExpSecs)
		redeemResp := decodeJSON(t, redeemRes)
		if redeemResp["fee_mloki"] == nil {
			t.Errorf("redeem(D) succeeded under a nonzero RedeemFeePpm hub but reported no fee_mloki field at all: %v", redeemResp)
		}
		if held[tokenD] {
			t.Errorf("BUG: token D reappears as held after being genuinely redeemed: held=%v", showResp["held_tokens"])
		}
		if counts["redeem"] == 0 {
			t.Errorf(`BUG: wallet history has no "redeem" entry despite redeem(D) succeeding — erased by the concurrent consolidate's Save: %v`, historyResp["history"])
		}
	} else {
		t.Logf("redeem(D) lost its race against the %ds expiry window (or was otherwise declined): %s", shortExpSecs, redeemRes.Stderr)
		// A cleanly-failed redeem must never partially apply — D stays
		// held, untouched, the same invariant TestCashTransfer_OverdraftAttempt
		// already establishes for a rejected overdraft.
		if !held[tokenD] {
			t.Errorf("a failed redeem(D) must leave it held, but it's missing from wallet show entirely: held=%v", showResp["held_tokens"])
		}
	}
}

// TestRace_ConcurrentReceiveSameToken fires two `cashctl receive` attempts
// against the exact same token string, same --config-dir, at the same
// instant — this round's headline open question. `receive`'s own wire call
// (checkClaimWithCashHub/CheckClaim) only QUERIES the Cash Hub, it never
// claims/consumes anything server-side the way redeem does, so both
// processes' CheckClaim calls can genuinely both succeed — nothing server-
// side stops them. Locally, both processes Load before either Saves (the
// same window CheckClaim's own network round trip opens up), so both see
// "not yet held" and both proceed to their own l.Add — and post-round-2,
// Save upserts by ID rather than rewriting the whole table. The open
// question: does that make it possible for both processes to successfully
// upsert two DIFFERENT entries (two different generated local IDs) for the
// SAME underlying token, silently double-counting its balance locally?
// Answer, confirmed live: no — entries.token's own UNIQUE constraint
// (internal/store's schema, independent of round 2's own fix) means the
// loser's INSERT collides on a column the ON CONFLICT(id) clause doesn't
// target, so SQLite reports a hard constraint violation and that whole
// Save is rolled back, not partially applied. See ledger_test.go's
// TestSave_ConcurrentSameTokenReceiveDoesNotDuplicate for the deterministic
// version of this same finding (and TestSave_ManyDisjointConcurrentWritersAllSucceed
// for the real bug this round DID find and fix along the way: without a
// busy-retry, SQLite write-lock contention alone — nothing to do with this
// token collision — could make Save fail outright even for fully disjoint
// writes).
func TestRace_ConcurrentReceiveSameToken(t *testing.T) {
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

	const amountMillis = uint64(75_000)
	hub := setUpCashHub(t, admin)
	token := mintPubkeyTokenFromHub(t, hub, myPubHex, amountMillis)

	results := runConcurrently(f,
		[]string{"receive", token},
		[]string{"receive", token},
	)

	successes := 0
	var loserStderr string
	for _, r := range results {
		if r.ExitCode == 0 {
			successes++
		} else {
			loserStderr = r.Stderr
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent receive of the identical token: %d of 2 succeeded, want exactly 1 (results: %+v)", successes, results)
	}
	// The loser must be told plainly this token is already held (exit 5,
	// CodeConflict — see AGENTS.md's error-code table), not a raw SQL
	// constraint message misclassified as an opaque internal failure.
	// Before this round's fix, ledger.Save's DB-level UNIQUE-constraint
	// catch of this exact race surfaced as a generic wrapped SQL error
	// (CodeInternal, exit 1) instead of ledger.ErrAlreadyHeld.
	if !strings.Contains(loserStderr, "already in your ledger") {
		t.Errorf("loser's stderr = %q, want the friendly ErrAlreadyHeld message, not a raw SQL constraint error", loserStderr)
	}

	// The ledger itself — not just "one command reported success" — must
	// show exactly one entry for this token. Two entries here would mean a
	// silently doubled local balance for money that only exists once.
	showResp := f.mustJSON("wallet", "show")
	held, _ := showResp["held_tokens"].([]any)
	matches := 0
	for _, h := range held {
		e, _ := h.(map[string]any)
		if tok, _ := e["token"].(string); tok == token {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("BUG: %d held-token entries for the same received token, want exactly 1 (double-counted local balance): %v", matches, showResp["held_tokens"])
	}

	historyResp := f.mustJSON("wallet", "history")
	if counts := historyActionCounts(t, historyResp); counts["receive"] != 1 {
		t.Errorf(`wallet history has %d "receive" entries for a concurrently-raced single token, want exactly 1: %v`, counts["receive"], historyResp["history"])
	}
}

// TestRace_ConcurrentJoinDifferentConfigDirs runs `cashctl join` from two
// genuinely separate --config-dirs (two distinct fixtures — this round's
// "two different users," not the shared-ledger race the rest of this file
// is about) against the SAME circle_hub at the same instant. cashctl's own
// local bookkeeping has nothing shared to race on here: appdir.override is
// a process-lifetime global set once from a flag (harmless across real OS
// processes, each with its own address space), identity generation
// (internal/identity.GenerateAndSaveLocal -> ncli.GenerateIdentity) draws
// from crypto/rand with no shared file/global state, and each fixture gets
// its own cashctl.db under its own --config-dir — confirmed by code
// reading (internal/appdir, internal/identity, internal/store) before
// writing this test. This is mostly a live sanity check that the SERVER
// copes with two genuinely concurrent create_circle_wallet calls against
// the same parent hub (lokihub's own audit suite is the actual authority
// on that) plus a confirmation that cashctl's own side has no surprise —
// both joins must succeed with two DISTINCT wallets, never blocking or
// corrupting each other.
func TestRace_ConcurrentJoinDifferentConfigDirs(t *testing.T) {
	cfg, err := LoadConfig("")
	if err != nil {
		t.Skipf("skipping: could not load integration config (%v) — see integration/README.md", err)
	}
	admin, ok := newAdminClient(cfg)
	if !ok {
		t.Skip("skipping: admin_api not configured — see integration/README.md")
	}

	userA := newFixture(t)
	userB := newFixture(t)
	initA := userA.mustJSON("wallet", "init")
	initB := userB.mustJSON("wallet", "init")
	pubHexA, err := npubToHex(initA["npub"].(string))
	if err != nil {
		t.Fatalf("decode user A identity npub: %v", err)
	}
	pubHexB, err := npubToHex(initB["npub"].(string))
	if err != nil {
		t.Fatalf("decode user B identity npub: %v", err)
	}
	if pubHexA == pubHexB {
		t.Fatalf("two independently-initialized fixtures generated the SAME identity — a real bug in identity generation, not this test")
	}

	// FundLoki well past setUpCircleHub's own single-join default (100 loki
	// == a 100_000 mloki hub budget, exactly enough for ONE --max-amount
	// 100000 reservation): two concurrent joins each reserving 100000
	// mloki from a shared circle-wide budget need enough headroom that
	// whichever commits second doesn't legitimately hit QUOTA_EXCEEDED —
	// that's a fixture-sizing question, not the local-bookkeeping race
	// this test actually targets.
	hub := setUpCircleHubOpts(t, admin, pubHexA, circleHubOpts{FundLoki: 500})
	if err := admin.addCircleAllowlistMember(hub.ID, pubHexB); err != nil {
		t.Fatalf("authorize user B under circle_hub allowlist: %v", err)
	}
	if hub.CircleHubToken == nil || *hub.CircleHubToken == "" {
		t.Fatalf("create ephemeral circle_hub: no circleHubToken in response: %+v", hub)
	}

	results := runConcurrentAcrossFixtures(
		fixtureCall{userA, []string{"join", "--hub", *hub.CircleHubToken, "--max-amount", "100000", "--yes"}},
		fixtureCall{userB, []string{"join", "--hub", *hub.CircleHubToken, "--max-amount", "100000", "--yes"}},
	)
	joinA, joinB := results[0], results[1]
	if joinA.ExitCode != 0 {
		t.Fatalf("concurrent join (user A): exit %d\nstdout: %s\nstderr: %s", joinA.ExitCode, joinA.Stdout, joinA.Stderr)
	}
	if joinB.ExitCode != 0 {
		t.Fatalf("concurrent join (user B): exit %d\nstdout: %s\nstderr: %s", joinB.ExitCode, joinB.Stdout, joinB.Stderr)
	}
	respA := decodeJSON(t, joinA)
	respB := decodeJSON(t, joinB)
	walletA, _ := respA["wallet"].(string)
	walletB, _ := respB["wallet"].(string)
	if walletA == "" || walletB == "" {
		t.Fatalf("join: missing wallet name(s): A=%q B=%q", walletA, walletB)
	}
	// walletA == walletB is EXPECTED, not a bug: "wallet" is a local
	// connection alias (config.SuggestName), derived from the hub's own
	// label — each fixture has its own separate connections table, so two
	// different users independently deriving the identical local name for
	// "the wallet I got from this circle" is no more a collision than two
	// different people both naming their own bank account "Checking." The
	// thing that actually needs to differ is the underlying server-side
	// wallet each name points at — WalletPubkey, from create_circle_wallet's
	// own NIP-44-encrypted, per-requester response.
	pubkeyA, _ := respA["response"].(map[string]any)["WalletPubkey"].(string)
	pubkeyB, _ := respB["response"].(map[string]any)["WalletPubkey"].(string)
	if pubkeyA == "" || pubkeyB == "" {
		t.Fatalf("join: missing response.WalletPubkey: A=%v B=%v", respA["response"], respB["response"])
	}
	if pubkeyA == pubkeyB {
		t.Errorf("BUG: both concurrent joins against the same circle_hub were handed the SAME underlying wallet (WalletPubkey %q) — a real collision, not just a shared local alias", pubkeyA)
	}

	// Each user's own side is independently live and only shows its own
	// wallet — no cross-contamination between the two --config-dirs.
	infoA := userA.mustJSON("wallet", "get-info")
	if infoA["alias"] == nil && infoA["methods"] == nil {
		t.Errorf("user A wallet get-info doesn't look like a get_info result: %v", infoA)
	}
	infoB := userB.mustJSON("wallet", "get-info")
	if infoB["alias"] == nil && infoB["methods"] == nil {
		t.Errorf("user B wallet get-info doesn't look like a get_info result: %v", infoB)
	}
}
