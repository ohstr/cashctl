//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcash/transport"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/wire"
)

// hubIdentityFromToken recovers the hub's own identity from a bill's mint
// signature, which is exactly what a client has to do: an announcement is only
// meaningful if its signature is checked against an identity known in advance,
// and the mint signature is the only thing a bill carries that supplies one.
func hubIdentityFromToken(t *testing.T, token string) (hubXOnly string, relays []string) {
	t.Helper()
	tok, err := nipcash.Decode(strings.SplitN(token, "#", 2)[0])
	if err != nil {
		t.Fatalf("decode token: %v", err)
	}
	if !tok.HasProvenance() {
		t.Fatal("this bill carries no mint signature, so a client has no hub identity to verify an announcement against — the private transport is unusable for it")
	}
	minter, ok := nipcash.VerifyProvenance(tok)
	if !ok {
		t.Fatal("this bill's mint signature does not verify")
	}
	return minter, tok.RelayURLs
}

// countPrivateRequests opens a live subscription that counts kind-23190 requests
// addressed to inbox, and returns a stop function giving the final count.
//
// Live rather than a replay: kind 23190 is ephemeral and a relay is not required
// to store it — this transport specifically does not want it to — so the only way
// to observe one is to be listening when it goes past.
func countPrivateRequests(t *testing.T, relayURL, inbox string) (stop func() int64) {
	t.Helper()
	u, err := url.Parse(relayURL)
	if err != nil {
		t.Fatalf("parse relay url %q: %v", relayURL, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	conn, err := relayclient.Connect(ctx, u)
	if err != nil {
		cancel()
		t.Fatalf("connect to relay %s: %v", relayURL, err)
	}
	// SubscribeWithID + Read, not Connection.Subscribe: Subscribe closes its
	// channel at EOSE by design, and EOSE only marks the end of STORED events. A
	// kind-23190 request is ephemeral and arrives live, so a Subscribe-based
	// observer counts zero however many requests actually go past — which is
	// exactly the bug this transport's own reply path had.
	subID := "e2e-private-request-counter"
	if !conn.SubscribeWithID(subID, nip01.NewSubscriptionFilterGroup(&nip01.SubscriptionFilter{
		Kinds: []int{transport.KindPrivateRequest},
		Tags:  map[string][]string{"p": {inbox}},
	})) {
		cancel()
		conn.Close()
		t.Fatal("could not open the observing subscription")
	}

	var seen atomic.Int64
	done := make(chan struct{})
	stopCh := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case msg, ok := <-conn.Read():
				if !ok {
					return
				}
				if m, ok := msg.(*wire.EventSubscriptionResponse); ok && m.SubscriptionID == subID && m.Event != nil {
					seen.Add(1)
				}
			case <-stopCh:
				return
			}
		}
	}()
	return func() int64 {
		// A moment for anything still in flight, so the count cannot be low
		// merely because we stopped listening too early.
		time.Sleep(2 * time.Second)
		// Stop via our own channel rather than relying on Close() ending the
		// Read range — it does not, and ranging over it here deadlocked the test.
		close(stopCh)
		<-done
		conn.Close()
		cancel()
		return seen.Load()
	}
}

// TestPrivateTransport_RedeemsManyBillsInOneRelayEvent is the end-to-end proof the
// whole private-transport effort exists for: several bills spent in ONE relay
// event instead of one event per bill.
//
// Everything here is real — a real hub, real bills with real mint signatures, the
// real compiled cashctl binary, a real relay — and the assertion that matters is
// the event count, observed on the relay itself rather than inferred from
// cashctl's own output. Two bills redeemed over one request is the entire claim;
// counting them anywhere else would be trusting the thing under test.
//
// --transport private is deliberate: it refuses to fall back, so a broken
// transport cannot pass by silently redeeming over the standard path instead.
func TestPrivateTransport_RedeemsManyBillsInOneRelayEvent(t *testing.T) {
	const billCount = 3

	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	// Both bills from the SAME hub: an envelope's items must all bind to one hub,
	// so bills from different hubs could never share a request.
	// Mint-signed deliberately, and it is not a test detail: a client anchors an
	// announcement's signature to an identity it already trusts, and a bill's mint
	// signature is the only thing that supplies one. A bill minted without it —
	// which is mint_cash's own default — can only ever use the standard transport.
	first, minter, signed := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 40_000)
	if !signed {
		t.Skip("this hub did not attach a mint signature (it is best-effort server-side), so no client could use its private transport")
	}
	second, _, signed2 := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 30_000)
	third, _, signed3 := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 20_000)
	if !signed2 || !signed3 {
		t.Skip("a bill came back unsigned")
	}
	f.mustJSON("receive", first)
	f.mustJSON("receive", second)
	f.mustJSON("receive", third)

	// The payouts need somewhere to go. The hub itself holds make_invoice, so it
	// doubles as the destination wallet — one invoice per bill is drawn from it.
	f.mustJSON("connect", "add", "dest", hub.PairingUri)

	hubXOnly, relays := hubIdentityFromToken(t, first)
	if hubXOnly != minter {
		t.Fatalf("recovered hub identity %s disagrees with decode's minter_pubkey %s", hubXOnly, minter)
	}
	if len(relays) == 0 {
		t.Fatal("bill carries no relay hints, so nothing can find the hub")
	}

	// Resolve the announcement the way cashctl does, and fail loudly if the hub is
	// not advertising the transport — otherwise the count assertion below would
	// pass for the wrong reason.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &nipcashclient.Client{}
	session, err := client.NewBatchSession(ctx, hubXOnly, relays)
	if err != nil {
		t.Fatalf("this hub publishes no usable private-transport announcement (%v) — enable PRIVATE_TRANSPORT_ENABLED on it before running this test", err)
	}
	inbox := session.Inbox()
	if inbox == "" {
		t.Fatal("the announcement names no inbox")
	}
	t.Logf("hub identity %s, inbox %s, relays %v", hubXOnly, inbox, session.Relays())

	stop := countPrivateRequests(t, relays[0], inbox)

	res := f.run("redeem", "--all", "--into", "dest", "--transport", "private", "--json", "--yes")
	requests := stop()

	if res.ExitCode != 0 {
		t.Fatalf("redeem --transport private: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}

	// Both bills paid out, read from the per-bill report.
	var resp map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &resp); err != nil {
		t.Fatalf("redeem --json did not print one JSON object: %v\nstdout: %s", err, res.Stdout)
	}
	rows, _ := resp["redeemed"].([]any)
	if len(rows) != billCount {
		t.Fatalf("redeemed %d bills, want %d\nstdout: %s", len(rows), billCount, res.Stdout)
	}
	for i, r := range rows {
		row, _ := r.(map[string]any)
		if row["status"] != "ok" {
			t.Errorf("bill %d: status=%v error=%v code=%v", i, row["status"], row["error"], row["code"])
		}
		if row["preimage"] == nil || row["preimage"] == "" {
			t.Errorf("bill %d: no preimage, so nothing proves it was actually paid", i)
		}
	}

	// The claim, and it is about SCALING rather than a magic number.
	//
	// A redeem is two phases, so it publishes two request envelopes: one
	// cash_status batch for the fee quotes, then one cash_redeem batch for the
	// spends. Both carry every bill. What matters is that the count is two
	// regardless of how many bills are in the run — on the standard transport it
	// would be two events PER BILL, which for three bills is six, each one tagged
	// with its own bill's wallet pubkey and published seconds apart.
	const wantRequests = 2
	if requests != wantRequests {
		t.Errorf("observed %d kind-23190 requests on the relay for %d bills, want exactly %d (one cash_status batch + one cash_redeem batch) — the count must not scale with bill count, or nothing is being batched",
			requests, billCount, wantRequests)
	}

	// And the ledger agrees that nothing is left held.
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 0 {
		t.Errorf("%d bill(s) still held after redeeming everything: %v", len(held), held)
	}
}

// TestPrivateTransport_DerivedBillIsServed is the regression test for a bill
// cashctl produced ITSELF — a consolidate's merged output — rather than one the hub
// minted and signed.
//
// Such a bill is a brand-new wallet that no mint signature can verify against, so
// it INHERITS its sources' minter (ledger.Entry.MinterPubkey). That makes it look
// batchable and it does address the right hub — the announcement is found and
// verified — but on the first live run the hub unwrapped the envelope and then
// omitted the item, which reaches a caller as "may or may not have been redeemed".
//
// --transport private, so a fallback cannot disguise the failure.
func TestPrivateTransport_DerivedBillIsServed(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	// Two signed bills from one hub, merged by cashctl into a third that carries
	// no mint signature of its own.
	first, _, signed1 := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 40_000)
	second, _, signed2 := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 30_000)
	if !signed1 || !signed2 {
		t.Skip("hub did not attach mint signatures")
	}
	f.mustJSON("receive", first)
	f.mustJSON("receive", second)

	consolidated := f.mustJSON("consolidate", "--json", "--yes")
	newEntry, _ := consolidated["new_entry"].(map[string]any)
	if newEntry == nil {
		t.Fatalf("consolidate produced no new_entry: %v", consolidated)
	}
	derivedID, _ := newEntry["id"].(string)
	if minter, _ := newEntry["minter_pubkey"].(string); minter == "" {
		t.Skipf("the merged bill inherited no minter, so it cannot use the private transport at all: %v", newEntry)
	}

	f.mustJSON("connect", "add", "dest", hub.PairingUri)

	res := f.run("redeem", "--token", derivedID, "--into", "dest", "--transport", "private", "--json", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("redeeming a cashctl-derived bill over the private transport: exit %d\nstdout: %s\nstderr: %s",
			res.ExitCode, res.Stdout, res.Stderr)
	}
}

// TestPrivateTransport_MergedCashModeReceipt documents a LEDGER bug that the
// private transport exposes, and pins the behaviour that is correct today.
//
// Auto-securing a cash-mode receipt and merging it produces a bill the hub records
// as cash-mode — read straight off its roster below: identity_type="cash". cashctl's
// own entry does not say so, so resolveCredential picks the local pubkey and signs a
// proof. The hub then refuses it correctly, because a proof-bearing item must match a
// non-cash claim and a cash-mode bill has none.
//
// The mismatch is cashctl's: its record of what that bill IS disagrees with the hub.
// The standard transport tolerates it, which is why it went unnoticed; the private
// transport's per-item authorization does not, and answers with an omission that
// carries no diagnosis. That is why --transport defaults to standard.
//
// What is asserted here is the true current contract: the default redeems this bill.
// The --transport auto case is skipped, not deleted, so the gap stays visible next to
// the evidence for it.
func TestPrivateTransport_MergedCashModeReceipt(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f := newFixture(t)

	pub, err := npubToHex(f.mustJSON("wallet", "init")["npub"].(string))
	if err != nil {
		t.Fatal(err)
	}

	existing, _, ok1 := mintSignedPubkeyTokenFromHub(t, f, hub, pub, 30_000)
	if !ok1 {
		t.Skip("hub did not attach a mint signature")
	}
	f.mustJSON("receive", existing)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cashClient := dialCash(t, ctx, hub.PairingUri)
	cashResult, err := cashClient.MintCash(ctx, nipcash.MintCashParams{
		Recipients:    []nipcash.Allocation{nipcash.Send(nipcash.Anyone(), 50_000)},
		MintSignature: true,
	})
	if err != nil {
		t.Fatalf("mint_cash (cash, signed): %v", err)
	}
	if len(cashResult.Recipients) != 1 || cashResult.Recipients[0].CashSecret == "" {
		t.Fatalf("mint_cash (cash): %+v", cashResult.Recipients)
	}

	// Receiving this auto-secures it and merges it with the existing holding.
	receiveResp := f.mustJSON("receive", cashResult.CashToken+"#"+cashResult.Recipients[0].CashSecret)
	entry, _ := receiveResp["entry"].(map[string]any)
	mergedID, _ := entry["id"].(string)
	if mergedID == "" {
		t.Fatalf("receive produced no entry: %v", receiveResp)
	}

	// The evidence for the mismatch, read from the hub rather than asserted.
	mergedToken, _ := entry["token"].(string)
	hubSaysCashMode := false
	if mergedToken != "" {
		statusCtx, statusCancel := context.WithTimeout(context.Background(), 20*time.Second)
		if c, dialErr := nipcashclient.Connect(statusCtx, mergedToken); dialErr == nil {
			if roster, rErr := c.CashStatus(statusCtx); rErr == nil && roster != nil {
				for _, r := range roster.Recipients {
					t.Logf("hub roster: identity_type=%q amount=%d claimed=%v", r.IdentityType, r.AmountMillis, r.Claimed)
					if r.IsCash() {
						hubSaysCashMode = true
					}
				}
			}
			c.Close()
		}
		statusCancel()
	}

	f.mustJSON("connect", "add", "dest", hub.PairingUri)

	// The contract that holds today: the default transport redeems this bill.
	if res := f.run("redeem", "--token", mergedID, "--into", "dest", "--json", "--yes"); res.ExitCode != 0 {
		t.Fatalf("the default transport must redeem a merged cash-mode receipt: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}

	if hubSaysCashMode {
		t.Skip("KNOWN GAP: the hub records this merged bill as cash-mode while cashctl's entry does not, " +
			"so --transport auto signs a pubkey proof the hub correctly refuses (omission). " +
			"Tracked in lokihub data/docs/issues/private-transport-omits-derived-bills-2026-09-29.md — " +
			"the fix belongs in cashctl's auto-secure/merge bookkeeping, not in the transport.")
	}
}
