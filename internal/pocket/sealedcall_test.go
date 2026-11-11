package pocket

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/ledger"
)

// a valid secp256k1 scalar for the test wallet key (encrypt-to-self).
const testPriv = "0101010101010101010101010101010101010101010101010101010101010101"

func testPub(t *testing.T) string {
	t.Helper()
	pub, err := utils.GetPublicKey(testPriv)
	if err != nil {
		t.Fatalf("pubkey: %v", err)
	}
	return pub
}

// TestSealedOrderWithdrawalRoundTrips proves ledger.Order and ledger.Withdrawal
// (both funds-bearing, both json:"-" secret fields + an AcceptedRelays slice gob
// loses empty-vs-nil on) seal/open byte-identical across nil / empty / non-empty
// slices, and leak no cleartext secret — the same encrypt-at-rest + fail-closed
// read-back guarantee the entry/call sealing has (risk 17 / finding #16).
func TestSealedOrderWithdrawalRoundTrips(t *testing.T) {
	priv, pub := testPriv, testPub(t)
	const secret = "order-notify-and-quote-secret-do-not-leak"

	for _, relays := range [][]string{nil, {}, {"wss://a.example", "wss://b.example"}} {
		o := ledger.Order{
			OrderID: "ord-0001", CallID: "call-0001", Notify: secret, Payment: "lightning",
			AmountMillis: 7000, RecipientKind: "pubkey", Recipient: "recip", HubIdentity: "hub",
			Chain: "flokicoin", Network: "regtest", ConnVerified: true, State: ledger.OrderPlacing,
			SawProgress: true, PayBy: 1760000300, OrderJSON: `{"order":"` + secret + `"}`,
			AcceptedRelays: relays, CreatedAt: 1760000000,
		}
		sc, err := sealOrder(o, priv, pub)
		if err != nil {
			t.Fatalf("sealOrder(relays=%v): %v", relays, err)
		}
		if strings.Contains(sc, secret) {
			t.Fatalf("sealed order leaks cleartext secret (relays=%v)", relays)
		}
		if got, err := openOrder(sc, priv, pub); err != nil {
			t.Fatalf("openOrder: %v", err)
		} else if !reflect.DeepEqual(got, o) {
			t.Fatalf("order round-trip mismatch (relays=%v):\n got  %+v\n want %+v", relays, got, o)
		}

		w := ledger.Withdrawal{
			WithdrawalID: "wd-0001", CallID: "call-0002", SourceEntryID: "tok-aaaa",
			WalletPubkey: "aaaa", HubIdentity: "hub", Address: "flc1qaddr", QuoteJSON: secret,
			QuoteID: "q1", AmountMillis: 6000, PayoutMillis: 5900, Notify: secret,
			AcceptedRelays: relays, State: "placing", QuoteExpiredSeen: false, ReceiptInvalidSeen: 0,
		}
		sw, err := sealWithdrawal(w, priv, pub)
		if err != nil {
			t.Fatalf("sealWithdrawal(relays=%v): %v", relays, err)
		}
		if strings.Contains(sw, secret) {
			t.Fatalf("sealed withdrawal leaks cleartext secret (relays=%v)", relays)
		}
		if got, err := openWithdrawal(sw, priv, pub); err != nil {
			t.Fatalf("openWithdrawal: %v", err)
		} else if !reflect.DeepEqual(got, w) {
			t.Fatalf("withdrawal round-trip mismatch (relays=%v):\n got  %+v\n want %+v", relays, got, w)
		}
	}
}

// TestSealedRoundTrips proves the faithful seal/open helpers (1) round-trip a
// ledger.Call / ledger.Entry / ledger.Handoff byte-identical and (2) leak no
// cleartext secret into the sealed blob (the plan's risk 17 — encrypt-at-rest).
func TestSealedRoundTrips(t *testing.T) {
	priv, pub := testPriv, testPub(t)
	const secret = "topsecretspendingmaterial-do-not-leak"

	call := ledger.Call{
		CallID: "abc123def456", RequestID: "reqid-0001", Method: "cash_transfer",
		Purpose: "transfer", Deadline: 1760000300, Target: "targetwallet",
		TargetCredential: "cred", AmountMillis: 5000, Chain: "flokicoin",
		AuthorizerKind: ledger.AuthCashSecret, AuthorizerSecret: secret,
		SourceEntryIDs: []string{"tok-aaaa", "tok-bbbb"}, ParamsJSON: `{"amount":5000}`,
		Attempt: 1, State: ledger.CallWritten,
		Secrets: []ledger.CallSecret{{Role: ledger.RoleRemainder, Secret: secret, Commitment: "commit"}},
	}
	amt := uint64(5000)
	entry := ledger.Entry{
		ID: "tok-aaaa", Token: "flokicash1abcd", WalletPubkey: "aaaa", AmountMillis: &amt,
		Status: ledger.StatusHeld, Verified: true, CashSecret: secret, ThrowawayKey: secret,
		MinterPubkey: ptr("minter"), CreatedByCall: "abc123def456",
	}
	handoff := ledger.Handoff{Handoff: "flokicash1recip#" + secret, Kind: ledger.HandoffGift, Status: ledger.HandoffSent, WalletPubkey: "recip", AmountMillis: 5000}

	// Call
	sc, err := sealCall(call, priv, pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sc, secret) {
		t.Fatal("sealed call leaks the cleartext secret")
	}
	if got, err := openCall(sc, priv, pub); err != nil {
		t.Fatal(err)
	} else if !reflect.DeepEqual(got, call) {
		t.Fatalf("call round-trip mismatch:\n got %+v\nwant %+v", got, call)
	}

	// Entry (the faithful ledger.Entry, with cashctl-only fields)
	se, err := sealEntry(entry, priv, pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(se, secret) {
		t.Fatal("sealed entry leaks the cleartext secret")
	}
	if got, err := openEntry(se, priv, pub); err != nil {
		t.Fatal(err)
	} else if !reflect.DeepEqual(got, entry) {
		t.Fatalf("entry round-trip mismatch:\n got %+v\nwant %+v", got, entry)
	}

	// Handoff
	sh, err := sealHandoff(handoff, priv, pub)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sh, secret) {
		t.Fatal("sealed handoff leaks the cleartext secret")
	}
	if got, err := openHandoff(sh, priv, pub); err != nil {
		t.Fatal(err)
	} else if !reflect.DeepEqual(got, handoff) {
		t.Fatalf("handoff round-trip mismatch:\n got %+v\nwant %+v", got, handoff)
	}
}

// TestCacheSealedCRUD proves the cache's sealed-blob tables store, list and
// delete the three faithful-state kinds.
func TestCacheSealedCRUD(t *testing.T) {
	c, err := OpenCache(filepath.Join(t.TempDir(), "cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.PutSealedCall("req-1", "sealedblobA", 100); err != nil {
		t.Fatal(err)
	}
	if err := c.PutSealedCall("req-1", "sealedblobA2", 101); err != nil { // upsert
		t.Fatal(err)
	}
	calls, err := c.SealedCalls()
	if err != nil {
		t.Fatal(err)
	}
	if calls["req-1"] != "sealedblobA2" {
		t.Fatalf("sealed call upsert: got %q", calls["req-1"])
	}
	if err := c.DeleteSealedCall("req-1"); err != nil {
		t.Fatal(err)
	}
	if calls, _ = c.SealedCalls(); len(calls) != 0 {
		t.Fatalf("delete left %d calls", len(calls))
	}

	if err := c.PutSealedEntry("tok-aaaa", "aaaa", "entryblob", 100); err != nil {
		t.Fatal(err)
	}
	if ents, _ := c.SealedEntries(); ents["tok-aaaa"] != "entryblob" {
		t.Fatalf("sealed entry: got %q", ents["tok-aaaa"])
	}

	id, err := c.PutSealedHandoff(0, "handoffblob", 100)
	if err != nil {
		t.Fatal(err)
	}
	if hs, _ := c.SealedHandoffs(); hs[id] != "handoffblob" {
		t.Fatalf("sealed handoff: got %q for id %d", hs[id], id)
	}
}

func ptr[T any](v T) *T { return &v }
