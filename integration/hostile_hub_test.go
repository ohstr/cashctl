//go:build integration

// hostile_hub_test.go drives the binary against a Hub that lies (fakehub_test.go):
// about a bill's amount or fee, about how many reply chunks are coming, or about
// where its private inbox is. None of this needs a live node, so it always runs.
package integration

import (
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash/transport"
	"github.com/ohstr/nmilat/utils"
)

// TestHostileHub_ReceiveRefusesMoreThanTheSignedBill: the Hub reports a
// slice larger than the whole bill its own mint signature attests.
func TestHostileHub_ReceiveRefusesMoreThanTheSignedBill(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		return statusReply(item, "pubkey", pub, 30_000)
	})
	bill, _ := hub.mint(25_000, "")
	requireFailure(t, "receive an inflated bill", f.run("receive", bill), 3, "invalid_input", "")
	if held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any); len(held) != 0 {
		t.Errorf("an inflated bill was saved: %v", held)
	}
}

// TestHostileHub_ReceiveNamesWhoIssuedTheBill: anyone can run a Hub and sign
// bills, so a received bill is only worth what its issuer will pay. cashctl
// never claims more than that: the entry records the issuer's key, which is
// what a user decides to trust or not.
func TestHostileHub_ReceiveNamesWhoIssuedTheBill(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		return statusReply(item, "pubkey", pub, 25_000)
	})
	bill, _ := hub.mint(25_000, "")
	entry, _ := requireOK(t, "receive", f.run("receive", bill))["entry"].(map[string]any)
	want := hex.EncodeToString(hub.minter.PubKey().SerializeCompressed())
	if got, _ := entry["minter_pubkey"].(string); got != want {
		t.Errorf("minter_pubkey = %q, want the issuing hub's key %q", got, want)
	}
}

// fakeWallet registers a destination wallet on the fake hub's relay that
// never answers, for a redeem that must be refused before it gets that far.
func fakeWallet(t *testing.T, f *fixture, hub *fakeHub) {
	t.Helper()
	uri := "nostr+walletconnect://" + fakeHex32(t) + "?relay=" + hub.url + "&secret=" + fakeHex32(t)
	requireOK(t, "connect add", f.run("connect", "add", "payout", uri))
}

// TestHostileHub_RedeemRefusesAFeeRaisedAfterReceive: a slice's redeem fee
// is fixed when it is minted, so a Hub quoting a larger one at redeem than at
// receive is trying to keep the difference.
func TestHostileHub_RedeemRefusesAFeeRaisedAfterReceive(t *testing.T) {
	f, pub := newMember(t)
	var fee atomic.Uint64
	fee.Store(100)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		if item.Method != "cash_status" {
			return nil
		}
		return quoteReply(item, "pubkey", pub, 25_000, fee.Load())
	})
	bill, _ := hub.mint(25_000, "")
	id := entryID(t, requireOK(t, "receive", f.run("receive", bill))["entry"])
	fakeWallet(t, f, hub)

	fee.Store(12_500)
	requireFailure(t, "redeem at a raised fee", f.run("redeem", "--token", id, "--into", "payout", "--yes"), 3, "invalid_input", "")
	if n := len(hub.itemsFor("cash_redeem")); n != 0 {
		t.Errorf("%d cash_redeem item(s) sent at the raised fee, want none", n)
	}
}

// TestHostileHub_LostReplyChunkIsIndeterminate: the Hub says its reply to a
// transfer comes in two chunks and only ever sends one. Whether the transfer
// happened is unknown, so cashctl must say so (indeterminate) and must not
// send it again.
func TestHostileHub_LostReplyChunkIsIndeterminate(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		if item.Method == "cash_status" {
			return statusReply(item, "pubkey", pub, 25_000)
		}
		return nil // the transfer's own result is "in the chunk that never comes"
	})
	hub.lieTotal, hub.lieTotalFor = 2, "cash_transfer"
	bill, _ := hub.mint(25_000, "")
	id := entryID(t, requireOK(t, "receive", f.run("receive", bill))["entry"])

	_, other := newIdentity(t)
	requireFailure(t, "transfer with a lost reply chunk", f.run("transfer", "10", other, "--token", id, "--yes"), 8, "indeterminate", "")
	if n := len(hub.itemsFor("cash_transfer")); n != 1 {
		t.Errorf("the transfer reached the hub %d times, want exactly once (never resent blind)", n)
	}
	if held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any); len(held) != 1 {
		t.Errorf("held_tokens after an indeterminate transfer = %v, want the bill still listed", held)
	}
}

// TestHostileHub_AnnouncementSignedByAnotherKeyIsRefused: the relay serves an
// announcement for the Hub's identity that the Hub never signed, naming an
// inbox the attacker reads. Nothing may be sent to it.
func TestHostileHub_AnnouncementSignedByAnotherKeyIsRefused(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		return statusReply(item, "pubkey", pub, 25_000)
	})
	attackerPriv := randomHex32(t)
	attackerInbox, err := utils.GetPublicKey(randomHex32(t))
	if err != nil {
		t.Fatal(err)
	}
	hub.announcements = func() []*nip01.Event {
		return []*nip01.Event{signedAnnouncement(t, attackerPriv, hub.hubXOnly, attackerInbox, hub.limits, 0)}
	}
	bill, _ := hub.mint(25_000, "")
	if res := f.run("receive", bill); res.ExitCode == 0 {
		t.Fatalf("receive trusted an announcement the hub never signed: %s", res.Stdout)
	}
	time.Sleep(time.Second)
	if n := hub.requestsTo(attackerInbox); n != 0 {
		t.Errorf("%d request(s) sent to the forged announcement's inbox", n)
	}
}

// TestHostileHub_OlderAnnouncementIsNotUsed: the relay lists a stale, genuinely
// signed announcement (naming an inbox since abandoned) ahead of the current one.
// The client uses the newest.
func TestHostileHub_OlderAnnouncementIsNotUsed(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		return statusReply(item, "pubkey", pub, 25_000)
	})
	staleInbox, err := utils.GetPublicKey(randomHex32(t))
	if err != nil {
		t.Fatal(err)
	}
	now := uint64(time.Now().Unix())
	hub.announcements = func() []*nip01.Event {
		return []*nip01.Event{
			signedAnnouncement(t, hub.hubPriv, hub.hubXOnly, staleInbox, hub.limits, now-3600),
			signedAnnouncement(t, hub.hubPriv, hub.hubXOnly, hub.inboxXOnly, hub.limits, now),
		}
	}
	bill, _ := hub.mint(25_000, "")
	requireOK(t, "receive", f.run("receive", bill))
	if n := hub.requestsTo(staleInbox); n != 0 {
		t.Errorf("%d request(s) sent to the stale announcement's inbox", n)
	}
	if n := hub.requestsTo(hub.inboxXOnly); n == 0 {
		t.Error("nothing reached the current inbox")
	}
}

// TestHostileHub_LocalRelayIsPointedOut: receive dials a pasted bill's relay
// at once, so a bill naming one on this machine (the fake hub runs on
// 127.0.0.1) is pointed out rather than dialed in silence.
func TestHostileHub_LocalRelayIsPointedOut(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		return statusReply(item, "pubkey", pub, 25_000)
	})
	bill, _ := hub.mint(25_000, "")
	res := f.runInteractive("", "receive", bill)
	if res.ExitCode != 0 {
		t.Fatalf("receive: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if !strings.Contains(res.Stderr, "your own machine or network") {
		t.Errorf("no note about the local relay:\n%s", res.Stderr)
	}
}
