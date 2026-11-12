package pocket

import (
	"context"
	"encoding/hex"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcash/client"
)

func unix() int64 { return time.Now().Unix() }

func openPocket(t *testing.T) *Pocket {
	t.Helper()
	priv, _ := testKey(t)
	p, err := Open(context.Background(), t.TempDir(), priv, "bitcoin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

// transferCall is a minimal valid cash_transfer PendingCall (Device/Chain are
// filled by WriteAhead from the journal's DeviceStore + default chain).
func transferCall(t *testing.T, amount uint64) nipcash.PendingCall {
	t.Helper()
	rid, err := nipcash.NewRequestID()
	if err != nil {
		t.Fatalf("request id: %v", err)
	}
	return nipcash.PendingCall{
		RequestID: rid,
		Method:    nipcash.MethodCashTransfer,
		Deadline:  unix() + 300,
		Target:    hexN(t, 64),
		Amount:    amount,
	}
}

// hexN returns n lowercase hex characters (a stand-in wallet pubkey / target).
func hexN(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n/2)
	for i := range b {
		b[i] = byte(i*7 + 1)
	}
	return hex.EncodeToString(b)
}

// liveBill is a minimal held bill entry. A live entry must carry a cash token
// (Validate rejects a cashless non-final entry) but does not verify its
// signature for storage, so a placeholder token exercises the fold/store wiring
// without a Hub mint (a real signed token is integration-tier — CC-24).
func liveBill(w string, amount uint64) nipCP.Entry {
	return nipCP.Entry{
		W: w, Rev: 1, State: nipCP.StateLive, CheckedAt: unix(),
		Amount: amount, Chain: "bitcoin",
		Identity: nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: w},
		Cash:     "satscash1" + w,
	}
}

func TestWriteAheadStoresAndFolds(t *testing.T) {
	p := openPocket(t)
	ctx := context.Background()
	pc := transferCall(t, 5000)

	if err := p.Journal.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		t.Fatalf("write-ahead: %v", err)
	}

	// The pending entry is durable in the event store...
	entries, err := p.allEntries(ctx)
	if err != nil {
		t.Fatalf("allEntries: %v", err)
	}
	if len(entries) != 1 || entries[0].W != pc.RequestID || entries[0].State != nipCP.StatePending {
		t.Fatalf("store: want one pending entry w=%s, got %+v", pc.RequestID, entries)
	}
	// ...and projected into the decoded cache as an open call.
	st, ok, err := p.Cache.State(pc.RequestID)
	if err != nil || !ok || st != string(nipCP.StatePending) {
		t.Fatalf("cache state: ok=%v st=%q err=%v, want pending", ok, st, err)
	}
	open, err := p.Cache.OpenCalls()
	if err != nil || len(open) != 1 || open[0] != pc.RequestID {
		t.Fatalf("open calls: %v err=%v", open, err)
	}
	// A pending call carries no spendable value: balance stays zero.
	bal, err := p.Cache.Balance()
	if err != nil || len(bal) != 0 {
		t.Fatalf("balance: %v err=%v, want empty", bal, err)
	}
}

func TestApplyFinalErrorClosesPending(t *testing.T) {
	p := openPocket(t)
	ctx := context.Background()
	pc := transferCall(t, 5000)
	if err := p.Journal.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		t.Fatalf("write-ahead: %v", err)
	}
	pending := mustOpenPending(t, p, pc.RequestID)

	// BAD_REQUEST: the call never committed — the pending entry is closed done,
	// sources untouched.
	out := nipCP.Outcome{Err: &client.FinalError{Method: pc.Method, Code: "BAD_REQUEST"}}
	if err := p.Apply(ctx, pending, nil, out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertState(t, p, pc.RequestID, nipCP.StateDone)

	// Exactly-once guard: the cache now resolves the call to done (the settle
	// driver reads State()==done and skips it), and ApplyResult over a done
	// pending is refused. (The raw store still holds the superseded pending wrap;
	// resolution discards it by rev — which is why the driver reads the resolved
	// State(), not raw wraps.)
	done := pending
	done.State = nipCP.StateDone
	if err := p.Apply(ctx, done, nil, out); err == nil {
		t.Fatalf("re-apply of a done pending: want ErrNotPending, got nil")
	}
}

func TestApplyTombstoneMarksSourceAndClosesPending(t *testing.T) {
	p := openPocket(t)
	ctx := context.Background()

	// A held source bill, then a transfer naming it whose reply is a tombstone
	// (the bill was already swept): the source goes to its mapped final state and
	// the pending closes done.
	src := liveBill(hexN(t, 64), 5000)
	if err := p.Add(ctx, src); err != nil {
		t.Fatalf("add source: %v", err)
	}
	assertState(t, p, src.W, nipCP.StateLive)

	pc := transferCall(t, 5000)
	pc.Target = src.W
	if err := p.Journal.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		t.Fatalf("write-ahead: %v", err)
	}
	pending := mustOpenPending(t, p, pc.RequestID)

	out := nipCP.Outcome{
		Err:   &nipcash.BillGone{Tombstone: nipcash.Tombstone{Status: "spent", Reason: "expired"}},
		GoneW: src.W,
	}
	if err := p.Apply(ctx, pending, []nipCP.Entry{src}, out); err != nil {
		t.Fatalf("apply: %v", err)
	}
	assertState(t, p, pc.RequestID, nipCP.StateDone)
	assertState(t, p, src.W, nipCP.StateExpired)
	// The source left the live set.
	if bal, _ := p.Cache.Balance(); len(bal) != 0 {
		t.Fatalf("balance after tombstone = %v, want empty", bal)
	}
}

func TestLookupMapsAction(t *testing.T) {
	p := openPocket(t)
	ctx := context.Background()
	pc := transferCall(t, 5000)
	if err := p.Journal.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		t.Fatalf("write-ahead: %v", err)
	}
	pending := mustOpenPending(t, p, pc.RequestID)

	// A lookup that answered "done" with a result → apply it.
	act, err := p.Lookup(ctx, pending, pc, client.Action{Kind: client.ActionClose, Result: []byte(`{"ok":true}`)})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if act.Kind != nipCP.PocketApply {
		t.Fatalf("done lookup → %v, want PocketApply", act.Kind)
	}
	// A refused lookup → close the pending, no replay.
	act, err = p.Lookup(ctx, pending, pc, client.Action{Kind: client.ActionCloseRefused, Reason: "RESTRICTED"})
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if act.Kind != nipCP.PocketClose || act.Done == nil || act.Done.State != nipCP.StateDone {
		t.Fatalf("refused lookup → %+v, want PocketClose+done", act)
	}
}

func TestRebuildFromStoreEqualsIncremental(t *testing.T) {
	p := openPocket(t)
	ctx := context.Background()

	p.mustWriteAhead(t, transferCall(t, 1000))
	p.mustWriteAhead(t, transferCall(t, 2000))
	if err := p.Add(ctx, liveBill(hexN(t, 64), 7000)); err != nil {
		t.Fatalf("add: %v", err)
	}

	before := snapshot(t, p)
	if err := p.RebuildFromStore(ctx); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after := snapshot(t, p)
	if before != after {
		t.Fatalf("rebuild changed the projection:\n before %s\n after  %s", before, after)
	}
}

func TestSchemaBumpRebuildsOnOpen(t *testing.T) {
	dir := t.TempDir()
	key, _ := testKey(t)
	ctx := context.Background()

	p, err := Open(ctx, dir, key, "bitcoin")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	pc := transferCall(t, 3000)
	if err := p.Journal.WriteAhead(ctx, []nipcash.PendingCall{pc}); err != nil {
		t.Fatalf("write-ahead: %v", err)
	}
	// Simulate an older cache (schema 0) by clearing the projection + version.
	if _, err := p.Cache.db.Exec(`DELETE FROM entries`); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if err := p.Cache.setSchemaVersion(0); err != nil {
		t.Fatalf("set version: %v", err)
	}
	p.Close()

	// Re-opening sees the stale version and rebuilds the projection from pocket.db.
	p2, err := Open(ctx, dir, key, "bitcoin")
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(p2.Close)
	st, ok, err := p2.Cache.State(pc.RequestID)
	if err != nil || !ok || st != string(nipCP.StatePending) {
		t.Fatalf("after rebuild: ok=%v st=%q err=%v, want pending restored", ok, st, err)
	}
	if v, _ := p2.Cache.SchemaVersion(); v != cacheSchemaVersion {
		t.Fatalf("schema version after rebuild = %d, want %d", v, cacheSchemaVersion)
	}
}

// --- helpers ---

func (p *Pocket) mustWriteAhead(t *testing.T, pc nipcash.PendingCall) {
	t.Helper()
	if err := p.Journal.WriteAhead(context.Background(), []nipcash.PendingCall{pc}); err != nil {
		t.Fatalf("write-ahead: %v", err)
	}
}

func mustOpenPending(t *testing.T, p *Pocket, w string) nipCP.Entry {
	t.Helper()
	all, err := p.allEntries(context.Background())
	if err != nil {
		t.Fatalf("allEntries: %v", err)
	}
	for _, e := range all {
		if e.W == w && e.State == nipCP.StatePending {
			return e
		}
	}
	t.Fatalf("no open pending entry for %s", w)
	return nipCP.Entry{}
}

func assertState(t *testing.T, p *Pocket, w string, want nipCP.State) {
	t.Helper()
	st, ok, err := p.Cache.State(w)
	if err != nil || !ok || st != string(want) {
		t.Fatalf("state(%s) = %q ok=%v err=%v, want %q", w, st, ok, err, want)
	}
}

// snapshot renders every cache entry's w/state/amount for equality checks.
func snapshot(t *testing.T, p *Pocket) string {
	t.Helper()
	entries, conflicts, err := p.Cache.Entries()
	if err != nil {
		t.Fatalf("entries: %v", err)
	}
	s := ""
	for _, e := range entries {
		s += e.W + ":" + string(e.State) + ":" + itoa(e.Amount) + ";"
	}
	for _, c := range conflicts {
		s += "conflict:" + c + ";"
	}
	return s
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var b []byte
	for u > 0 {
		b = append([]byte{byte('0' + u%10)}, b...)
		u /= 10
	}
	return string(b)
}
