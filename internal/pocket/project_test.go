package pocket

import (
	"context"
	"testing"

	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// resolvedPocketDB reads pocket.db and returns the resolved NIP-CP winners by w
// plus the raw event count (to assert idempotence — no duplicate wraps).
func resolvedPocketDB(t *testing.T, p *Pocket) (map[string]nipCP.Entry, int) {
	t.Helper()
	wraps, err := p.Store.Query(context.Background(), localRelay, nipCP.FullSyncFilter(p.userPub))
	if err != nil {
		t.Fatalf("query pocket.db: %v", err)
	}
	all, err := nipCP.OpenAll(wraps, p.userPriv)
	if err != nil {
		t.Fatalf("open pocket.db wraps: %v", err)
	}
	winners, _ := nipCP.Resolve(all)
	return winners, len(wraps)
}

// TestProjectHeldBillToPocketDB: a held bill projects to a PURE NIP-CP 9525 in
// pocket.db (resolvable + correct), a spend supersedes it to spent by rev, and a
// re-run is idempotent (no duplicate wraps).
func TestProjectHeldBillToPocketDB(t *testing.T) {
	priv, _ := testKey(t)
	dir := t.TempDir()
	ctx := context.Background()

	p, err := Open(ctx, dir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open pocket: %v", err)
	}
	defer p.Close()

	amt := uint64(4200)
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", AttestedAmountMillis: &amt}, "")
	w, err := billform.WalletPubkey(tok)
	if err != nil {
		t.Fatalf("wallet pubkey: %v", err)
	}
	held := ledger.Entry{Token: tok, WalletPubkey: w, IdentityPubkey: w, AmountMillis: &amt, Status: ledger.StatusHeld}

	// Project the held bill → pocket.db.
	if err := p.Project(ctx, []ledger.Entry{held}); err != nil {
		t.Fatalf("project held: %v", err)
	}
	winners, n := resolvedPocketDB(t, p)
	got, ok := winners[w]
	if !ok {
		t.Fatalf("pocket.db has no entry for w=%s after projecting a held bill", w)
	}
	if got.State != nipCP.StateLive {
		t.Errorf("state = %q, want live", got.State)
	}
	if got.Amount != amt {
		t.Errorf("amount = %d, want %d", got.Amount, amt)
	}
	if got.Cash != tok {
		t.Errorf("cash token mismatch")
	}
	// PURE NIP-CP by construction: it round-trips through OpenAll/Resolve (above),
	// i.e. it is a plain kind-9525 with no cashctl-extension tags a NIP-CP wallet
	// couldn't read.

	// Idempotent: re-projecting the unchanged held bill writes nothing new.
	if err := p.Project(ctx, []ledger.Entry{held}); err != nil {
		t.Fatalf("re-project held: %v", err)
	}
	if _, n2 := resolvedPocketDB(t, p); n2 != n {
		t.Errorf("re-project changed wrap count %d -> %d (not idempotent)", n, n2)
	}

	// Spend → the projection supersedes the live entry with spent, by rev.
	held.Status = ledger.StatusSpent
	if err := p.Project(ctx, []ledger.Entry{held}); err != nil {
		t.Fatalf("project spent: %v", err)
	}
	winners, _ = resolvedPocketDB(t, p)
	if winners[w].State != nipCP.StateSpent {
		t.Errorf("after spend, state = %q, want spent", winners[w].State)
	}
	if winners[w].Rev <= got.Rev {
		t.Errorf("spent rev %d not > live rev %d", winners[w].Rev, got.Rev)
	}
}
