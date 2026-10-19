package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// identityMergeFixture is two identity-bound bills (1000 + 2000) under a fresh
// local identity, with the merge and the send faked: the merge lands as
// "merged-tok" for 3000, and send answers whatever the test says.
func identityMergeFixture(t *testing.T, send func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error)) (*ledger.Ledger, []string, []nipcash.Source, []string, string, string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	priv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	myPub, _ := utils.GetPublicKey(priv)

	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	var tokens, ids []string
	var sources []nipcash.Source
	for _, e := range []ledger.Entry{
		{Token: "tok-a", WalletPubkey: "pa", AmountMillis: ptrTo(uint64(1000)), IdentityRequired: ptrTo(true), Verified: true},
		{Token: "tok-b", WalletPubkey: "pb", AmountMillis: ptrTo(uint64(2000)), IdentityRequired: ptrTo(true), Verified: true},
	} {
		added, err := l.Add(e)
		if err != nil {
			t.Fatal(err)
		}
		tokens, ids = append(tokens, e.Token), append(ids, added.ID)
		sources = append(sources, nipcash.Source{WalletPubkey: e.WalletPubkey, Amount: *e.AmountMillis, Credential: nipcash.BySigning(priv)})
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}

	origCap, origMerge, origSend := hubConsolidateCapFn, attemptCashConsolidateFn, sendFromEntryFn
	t.Cleanup(func() { hubConsolidateCapFn, attemptCashConsolidateFn, sendFromEntryFn = origCap, origMerge, origSend })
	hubConsolidateCapFn = func(string) int { return 10 }
	attemptCashConsolidateFn = func(_ string, _ []nipcash.Source, target nipcash.Target) (*nipcash.CashConsolidateResult, error) {
		if target != nipcash.Pubkey(myPub) {
			t.Errorf("merged to %v, want the local identity", target)
		}
		return &nipcash.CashConsolidateResult{AmountMillis: 3000, NewWalletToken: "merged-tok", NewWalletPubkey: "pm"}, nil
	}
	sendFromEntryFn = send
	return l, tokens, sources, ids, priv, myPub
}

func heldByToken(t *testing.T, token string) (ledger.Entry, bool) {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range l.Entries {
		if e.Token == token {
			return e, e.Status == ledger.StatusHeld
		}
	}
	return ledger.Entry{}, false
}

var aliceTarget = credential.ResolvedTarget{Target: nipcash.Pubkey(strings.Repeat("a1", 32)), Kind: credential.TargetKindPubkey}

// #96: the merged bill is saved before the send goes out. A send with no answer
// leaves it held on disk, so a process that dies here loses nothing: the sources
// are consumed, and their merged value is still listed.
func TestMergeThenSend_IdentityPathSavesTheMergedBillBeforeSending(t *testing.T) {
	var onDiskAtSend bool
	l, tokens, sources, ids, _, pub := identityMergeFixture(t, func(e *ledger.Entry, _ nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		_, onDiskAtSend = heldByToken(t, "merged-tok")
		return nil, context.DeadlineExceeded
	})
	err := mergeThenSend(newTestTransferCmd(), l, tokens, sources, ids, 3000, 1000, "", pub, "alice", aliceTarget)
	if output.AsCLIError(err).Code != output.CodeIndeterminate {
		t.Fatalf("error = %v, want indeterminate", err)
	}
	if !onDiskAtSend {
		t.Error("the merged bill was not saved before the send was placed")
	}
	if e, held := heldByToken(t, "merged-tok"); !held || e.ThrowawayKey != "" {
		t.Errorf("merged bill after an unanswered send: held=%v %+v; want held, identity-bound", held, e)
	}
	for _, tok := range []string{"tok-a", "tok-b"} {
		if _, held := heldByToken(t, tok); held {
			t.Errorf("source %s still held after the merge consumed it", tok)
		}
	}
}

// A refused send leaves the merged bill held and identity-bound: it is never
// turned into a cash note, which only a throwaway-key merge does.
func TestMergeThenSend_IdentityPathKeepsTheMergedBillOnARefusal(t *testing.T) {
	l, tokens, sources, ids, _, pub := identityMergeFixture(t, func(*ledger.Entry, nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		return nil, &relayclient.WalletError{Code: "RESTRICTED", Message: "no"}
	})
	if err := mergeThenSend(newTestTransferCmd(), l, tokens, sources, ids, 3000, 1000, "", pub, "alice", aliceTarget); err == nil {
		t.Fatal("a refused send reported success")
	}
	e, held := heldByToken(t, "merged-tok")
	if !held || isCashNote(&e) || e.ThrowawayKey != "" || e.CashSecret != "" {
		t.Errorf("merged bill after a refusal: held=%v %+v; want held, identity-bound, not a cash note", held, e)
	}
}

// The send is signed by the local identity, which the merged bill is bound to —
// never by --as, which names the sources' credential.
func TestMergeThenSend_IdentityPathSendsAsTheLocalIdentity(t *testing.T) {
	var used nipcash.Credential
	l, tokens, sources, ids, priv, pub := identityMergeFixture(t, func(_ *ledger.Entry, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
		used = p.Credential
		return &nipcash.CashTransferResult{AmountMillis: 1000, NewWalletToken: "alice-tok", RemainingAmountMillis: ptrTo(uint64(2000)), RemainderWalletToken: "rem-tok"}, nil
	})
	c := newTestTransferCmd()
	if err := c.Flags().Set("as", "pubkey:"+strings.Repeat("1", 64)); err != nil {
		t.Fatal(err)
	}
	if err := mergeThenSend(c, l, tokens, sources, ids, 3000, 1000, "", pub, "alice", aliceTarget); err != nil {
		t.Fatalf("mergeThenSend() error = %v", err)
	}
	if used != nipcash.BySigning(priv) {
		t.Errorf("send signed with %v, want the local identity", used)
	}
	if _, held := heldByToken(t, "merged-tok"); held {
		t.Error("the merged bill is still held after it was spent")
	}
	if _, held := heldByToken(t, "rem-tok"); !held {
		t.Error("the remainder was not saved as a held bill")
	}
}
