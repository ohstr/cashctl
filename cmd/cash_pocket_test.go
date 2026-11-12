package cmd

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// fakeRelays is an in-memory nipCP.Relays for sync tests — no network. All wraps
// in a test are addressed to the one user key, so it matches on kind + since (the
// #p is implicitly satisfied); that's enough to exercise the cursor window.
type fakeRelays struct{ evs map[string]*nip01.Event }

func newFakeRelays() *fakeRelays { return &fakeRelays{evs: map[string]*nip01.Event{}} }

func (f *fakeRelays) Publish(_ context.Context, _ string, ev *nip01.Event) error {
	f.evs[ev.ID] = ev
	return nil
}

func (f *fakeRelays) Query(_ context.Context, _ string, flt nip01.SubscriptionFilter) ([]*nip01.Event, error) {
	var out []*nip01.Event
	for _, ev := range f.evs {
		if len(flt.Kinds) > 0 {
			ok := false
			for _, k := range flt.Kinds {
				if ev.Kind == k {
					ok = true
				}
			}
			if !ok {
				continue
			}
		}
		if flt.Since != 0 && uint64(ev.CreatedAt) < flt.Since {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// heldBill mints a fresh held-bill ledger.Entry via testmint.
func heldBill(t *testing.T, amt uint64) (ledger.Entry, string) {
	t.Helper()
	// A pubkey-bound bill is addressed to the USER's identity key (the key that
	// signs for it) — not the wallet pubkey. The pocket is encrypt-to-self to that
	// same key, so a synced bill bound to it is the user's own + spendable.
	userPub, err := utils.GetPublicKey(testPocketKey)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	tok := testmint.Bill(t, nipcash.Token{HRP: "lokicash", AttestedAmountMillis: &amt}, "")
	w, err := billform.WalletPubkey(tok)
	if err != nil {
		t.Fatalf("wallet pubkey: %v", err)
	}
	return ledger.Entry{Token: tok, WalletPubkey: w, IdentityPubkey: userPub, AmountMillis: &amt, Status: ledger.StatusHeld}, w
}

// TestCashPocketSync_SpendableAfterPull — the P5 completion: a bill synced A→B
// must land in B's AUTHORITATIVE sealed ledger (Held()), not just the decoded
// cache, so the money commands can spend it. Also asserts the import is idempotent.
func TestCashPocketSync_SpendableAfterPull(t *testing.T) {
	priv := testPocketKey
	userPub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	ctx := context.Background()
	relay := newFakeRelays()
	amt := uint64(7000)
	held, w := heldBill(t, amt)

	// A: mint locally + sync (publish to the relay).
	pA, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	if _, err := syncPocket(ctx, pA, []ledger.Entry{held}, priv, userPub, relay, []string{"r1"}, 1); err != nil {
		t.Fatalf("A sync: %v", err)
	}
	pA.Close()

	// B: same key, fresh pocket dir, pull.
	bdir := t.TempDir()
	pB, err := pocket.Open(ctx, bdir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	resB, err := syncPocket(ctx, pB, nil, priv, userPub, relay, []string{"r1"}, 2)
	if err != nil {
		t.Fatalf("B sync: %v", err)
	}
	pB.Close()
	if resB.Spendable < 1 {
		t.Fatalf("B spendable = %d, want >= 1 (synced bill must be made spendable)", resB.Spendable)
	}

	// B's AUTHORITATIVE sealed ledger must hold the bill, bound to the user's key.
	lsB, err := pocket.OpenLedgerStore(ctx, bdir, priv, ledger.LokiHRP)
	if err != nil {
		t.Fatalf("B ledger open: %v", err)
	}
	defer lsB.Close()
	var found *ledger.Entry
	for _, e := range lsB.Held() {
		if e.WalletPubkey == w {
			ee := e
			found = &ee
			break
		}
	}
	if found == nil {
		t.Fatalf("B's sealed ledger (Held) has no bill for w=%s — not spendable", w)
	}
	if found.Token != held.Token {
		t.Errorf("token = %q, want %q", found.Token, held.Token)
	}
	if found.IdentityPubkey != userPub {
		t.Errorf("identity = %q, want user %q (must be spendable by the user's key)", found.IdentityPubkey, userPub)
	}

	// Idempotent: a re-sync adds no duplicate to the sealed ledger.
	pB2, err := pocket.Open(ctx, bdir, priv, "flokicoin")
	if err != nil {
		t.Fatalf("reopen B: %v", err)
	}
	res2, err := syncPocket(ctx, pB2, nil, priv, userPub, relay, []string{"r1"}, 3)
	if err != nil {
		t.Fatalf("B re-sync: %v", err)
	}
	pB2.Close()
	if res2.Spendable != 0 {
		t.Errorf("re-sync spendable = %d, want 0 (import must be idempotent)", res2.Spendable)
	}
}

func entryFor(t *testing.T, p *pocket.Pocket, w string) (nipCP.Entry, bool) {
	t.Helper()
	entries, _, err := p.Cache.Entries()
	if err != nil {
		t.Fatalf("cache entries: %v", err)
	}
	for _, e := range entries {
		if e.W == w {
			return e, true
		}
	}
	return nipCP.Entry{}, false
}

// TestCashPocketSync_AtoB: device A projects + publishes a held bill to a (fake)
// relay; device B — same key, empty pocket — pulls it and folds it into its
// cache, and advances its cursor. This is the P5 client-pull path.
func TestCashPocketSync_AtoB(t *testing.T) {
	priv := testPocketKey
	userPub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	ctx := context.Background()
	relay := newFakeRelays()
	amt := uint64(5000)
	held, w := heldBill(t, amt)

	pA, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer pA.Close()
	resA, err := syncPocket(ctx, pA, []ledger.Entry{held}, priv, userPub, relay, []string{"r1"}, 1_000_000)
	if err != nil {
		t.Fatalf("A sync: %v", err)
	}
	if resA.Published < 1 {
		t.Fatalf("A published %d, want >= 1", resA.Published)
	}

	pB, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer pB.Close()
	resB, err := syncPocket(ctx, pB, nil, priv, userPub, relay, []string{"r1"}, 1_000_100)
	if err != nil {
		t.Fatalf("B sync: %v", err)
	}
	if resB.Imported < 1 {
		t.Fatalf("B imported %d, want >= 1", resB.Imported)
	}

	got, ok := entryFor(t, pB, w)
	if !ok {
		t.Fatalf("B's cache has no entry for A's bill w=%s", w)
	}
	if got.State != nipCP.StateLive || got.Amount != amt {
		t.Errorf("B entry = {state:%s amount:%d}, want {live %d}", got.State, got.Amount, amt)
	}
	if cur, _ := pB.Cache.SyncCursor(); cur != 1_000_100 {
		t.Errorf("B cursor = %d, want advanced to 1000100", cur)
	}
}

// TestCashPocketSync_CursorWindow: B syncs with a cursor set ABOVE the wrap's
// created_at — the pull must still catch it, because IncrementalFilter starts the
// query SyncOverlap (2 days) before the cursor (risk 13: NIP-59 back-dates wraps
// up to 2 days). With a 0 window (since = cursor) the wrap would be missed.
func TestCashPocketSync_CursorWindow(t *testing.T) {
	priv := testPocketKey
	userPub, _ := utils.GetPublicKey(priv)
	ctx := context.Background()
	relay := newFakeRelays()
	amt := uint64(7000)
	held, w := heldBill(t, amt)

	// A publishes at now=1_000_000 → wrap created_at is in [now-2d, now], i.e. <= 1_000_000.
	pA, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer pA.Close()
	if _, err := syncPocket(ctx, pA, []ledger.Entry{held}, priv, userPub, relay, []string{"r1"}, 1_000_000); err != nil {
		t.Fatalf("A sync: %v", err)
	}

	// B's cursor is set to 1_000_100 (> the wrap's created_at). since = 1_000_100 -
	// 172_800 = 827_300 <= the wrap ⇒ caught. (since = 1_000_100 would miss it.)
	pB, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer pB.Close()
	if err := pB.Cache.SetSyncCursor(1_000_100); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	res, err := syncPocket(ctx, pB, nil, priv, userPub, relay, []string{"r1"}, 1_000_200)
	if err != nil {
		t.Fatalf("B sync: %v", err)
	}
	if res.Imported < 1 {
		t.Fatalf("B imported %d, want >= 1 — the 2-day window should catch a wrap below the cursor", res.Imported)
	}
	if _, ok := entryFor(t, pB, w); !ok {
		t.Errorf("B's cache missing the back-dated bill w=%s (2-day window not applied?)", w)
	}
}

// TestCashPocketExportImport: the data flow the export/import commands run — A
// projects its held bills to pure 9525 wraps, those are JSON-serialised (export),
// and re-loaded into B's pocket (import), preserving the bill. (The cobra
// handlers wrap exactly this over store.Path()/--config-dir.)
func TestCashPocketExportImport(t *testing.T) {
	priv := testPocketKey
	userPub, _ := utils.GetPublicKey(priv)
	ctx := context.Background()
	amt := uint64(9000)
	held, w := heldBill(t, amt)

	// Export: A projects, then serialises its pocket.db wraps.
	pA, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	defer pA.Close()
	if err := pA.Project(ctx, []ledger.Entry{held}); err != nil {
		t.Fatalf("project: %v", err)
	}
	wraps, err := pA.Store.Query(ctx, pocket.LocalRelay, nipCP.FullSyncFilter(userPub))
	if err != nil {
		t.Fatalf("query A: %v", err)
	}
	blob, err := json.Marshal(wraps)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Import: B loads the bundle.
	pB, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	defer pB.Close()
	var in []*nip01.Event
	if err := json.Unmarshal(blob, &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, ev := range in {
		if err := pB.Store.Publish(ctx, pocket.LocalRelay, ev); err != nil {
			t.Fatalf("B store: %v", err)
		}
	}
	entries, err := nipCP.OpenAll(in, priv)
	if err != nil {
		t.Fatalf("open bundle: %v", err)
	}
	for _, e := range entries {
		if _, err := pB.Cache.Fold(e); err != nil {
			t.Fatalf("B fold: %v", err)
		}
	}

	got, ok := entryFor(t, pB, w)
	if !ok || got.Amount != amt || got.State != nipCP.StateLive {
		t.Errorf("after export→import, B entry = {%v %s %d}, want the live %d bill", ok, got.State, got.Amount, amt)
	}
	bal, _ := pB.Cache.Balance()
	var total uint64
	for _, v := range bal {
		total += v
	}
	if total != amt {
		t.Errorf("imported balance = %d, want %d", total, amt)
	}
}

// TestCashPocketServe_OwnerSyncsOffItAndNonOwnerRefused serves pocket A over a
// loopback relay (P6) and proves: (a) B, on the SAME identity key, syncs off it
// via a real WebSocket and the bill becomes spendable in B's sealed ledger;
// (b) a NON-owner key is refused the kind-1059 wraps by the recipient-only AUTH.
func TestCashPocketServe_OwnerSyncsOffItAndNonOwnerRefused(t *testing.T) {
	priv := testPocketKey
	userPub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const amt = 40_000
	held, w := heldBill(t, amt)

	// A: project the held bill into pocket.db and serve it on an ephemeral port.
	// Listen first so the handler's NIP-42 relay URL matches the dialed address.
	pA, err := pocket.Open(ctx, t.TempDir(), priv, "flokicoin")
	if err != nil {
		t.Fatal(err)
	}
	defer pA.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + ln.Addr().String()
	handler, err := pocketServeHandler(ctx, pA, []ledger.Entry{held}, url)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.VerificationWorker.Stop()
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	// B (same key, fresh dir): sync off A's serve → imports the bill, spendable.
	bdir := t.TempDir()
	pB, err := pocket.Open(ctx, bdir, priv, "flokicoin")
	if err != nil {
		t.Fatal(err)
	}
	resB, err := syncPocket(ctx, pB, nil, priv, userPub, nipCP.DialRelays(priv), []string{url}, time.Now().Unix())
	pB.Close()
	if err != nil {
		t.Fatalf("B sync off the serve: %v", err)
	}
	if resB.Imported < 1 {
		t.Fatalf("B imported %d off the serve, want >=1", resB.Imported)
	}
	lsB, err := pocket.OpenLedgerStore(ctx, bdir, priv, ledger.LokiHRP)
	if err != nil {
		t.Fatal(err)
	}
	defer lsB.Close()
	if _, ok := lsB.FindByToken(held.Token); !ok {
		t.Fatalf("served bill (w=%s) not in B's sealed ledger — not spendable", w)
	}

	// Non-owner: a DIFFERENT key (testPocketKey is 1111…11) is refused the owner's
	// kind-1059 wraps by the recipient-only AUTH gate.
	const otherPriv = "2222222222222222222222222222222222222222222222222222222222222222"
	evs, err := nipCP.DialRelays(otherPriv).Query(ctx, url, nipCP.FullSyncFilter(userPub))
	if err == nil && len(evs) > 0 {
		t.Errorf("non-owner got %d of the owner's 1059 wraps, want 0 (recipient-only AUTH must refuse)", len(evs))
	}
}
