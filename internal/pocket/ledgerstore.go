package pocket

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
)

// LedgerStore is the pocket-backed implementation of ledger.Store — approach A'
// of the pocket-ledger migration (the plan's §8). Rather than re-implement the
// 24 ledger methods over the event-sourced pocket (which would have to emulate the
// old ledger's load/mutate-pointers/Save-flush lifecycle and would silently drop
// cmd's in-place mutations, or lose the random ids that Call.SourceEntryIDs
// reference — a double-spend hazard), it EMBEDS a real in-memory *ledger.Ledger as
// the working buffer: every read/mutation (Add/Find/Held/SetStatus/AddCall/
// FindCall/OpenCallFor/AddHandoff/...) is the battle-tested in-memory method,
// reused UNCHANGED, so ids + linkage + every cashctl-only field are preserved by
// construction.
//
// Only persistence swaps:
//   - Save() (below) persists the embedded ledger to the pocket instead of
//     cashctl.db — one atomic transaction over the NIP-44-sealed faithful tables
//     (entries_full/pending_calls/handoffs), encrypt-to-self so no secret is ever
//     cleartext at rest (the plan's risk 17).
//   - Open loads the in-memory ledger back from those sealed tables, byte-identical.
//   - the derived-id gate (InsertDerived/...) goes to pocket-cache.db's derived_ids
//     (overriding the embedded ledger's cashctl.db-backed ones).
//
// The pure-NIP-CP 9525 projection into pocket.db (the universal/syncable copy that
// P5 sync / P6 serve consume) is a tracked follow-up — see Save. It is NOT needed
// for cmd correctness: the sealed tables are cmd's source of truth and reload the
// in-memory ledger faithfully.
type LedgerStore struct {
	*ledger.Ledger
	p   *Pocket
	ctx context.Context
	// version is the store_version this store loaded at (optimistic concurrency,
	// #14). Save passes it to PersistLedger's compare-and-set and advances it on
	// success, so a concurrent process that wrote in between is refused rather than
	// clobbered.
	version int64
}

var _ ledger.Store = (*LedgerStore)(nil)

// OpenLedgerStore opens the pocket under dir and returns it as a ledger.Store with
// the in-memory ledger reconstructed from the faithful sealed tables. userPriv is
// the wallet key (the pocket is encrypted to it); chain is the default chain
// stamped on a call that carries none.
func OpenLedgerStore(ctx context.Context, dir, userPriv, chain string) (*LedgerStore, error) {
	// Cache-only (sqlite): the A′ ledger path never touches the bbolt pocket.db
	// event store, so opening it would take its single-writer lock for nothing and
	// deadlock the nested loads withdraw/order make. ctx/chain are accepted for
	// signature stability but unused here (no event store, no journal).
	p, err := OpenLedgerOnly(dir, userPriv)
	if err != nil {
		return nil, err
	}
	l, err := loadLedger(p)
	if err != nil {
		p.Close()
		return nil, err
	}
	version, err := p.Cache.StoreVersion()
	if err != nil {
		p.Close()
		return nil, err
	}
	return &LedgerStore{Ledger: l, p: p, ctx: ctx, version: version}, nil
}

// Close releases the underlying pocket (the bbolt single-writer lock + the
// cache) — a command MUST defer this so the next command can open the store.
func (s *LedgerStore) Close() error { s.p.Close(); return nil }

// ZapEventFor returns the Cash Zap (kind-9522) stored with the held bill a token
// spells, "" when none — the pocket equivalent of the package-level
// ledger.ZapEventFor, read over the embedded ledger's faithful entries instead of
// cashctl.db. It picks the same row that func's ORDER BY would: a live
// (held/redeeming/zap_claiming) row first, then a non-duplicate, then the latest.
func (s *LedgerStore) ZapEventFor(token string) (string, error) {
	w, err := billform.WalletPubkey(token)
	if err != nil {
		return "", nil
	}
	best := -1
	for i := range s.Ledger.Entries {
		if s.Ledger.Entries[i].WalletPubkey != w {
			continue
		}
		if best < 0 || zapRowBetter(s.Ledger.Entries[i], i, s.Ledger.Entries[best], best) {
			best = i
		}
	}
	if best < 0 {
		return "", nil
	}
	return s.Ledger.Entries[best].ZapEvent, nil
}

// zapRowBetter reports whether entry a (at slice index ai) outranks b (bi) under
// ledger.ZapEventFor's order: a live status first, then a non-duplicate, then the
// latest row (higher slice index ≈ higher rowid).
func zapRowBetter(a ledger.Entry, ai int, b ledger.Entry, bi int) bool {
	if al, bl := ledger.Live(a.Status), ledger.Live(b.Status); al != bl {
		return al
	}
	if ad, bd := a.Status == ledger.StatusDuplicate, b.Status == ledger.StatusDuplicate; ad != bd {
		return !ad
	}
	return ai > bi
}

// loadLedger reconstructs the in-memory *ledger.Ledger from the faithful sealed
// tables (byte-identical: ids, linkage, and the cashctl-only fields the NIP-CP
// 9525 cannot carry). Order is deterministic (sorted by id) so a reload is stable.
func loadLedger(p *Pocket) (*ledger.Ledger, error) {
	priv, pub := p.userPriv, p.userPub

	sealedEntries, err := p.Cache.SealedEntries()
	if err != nil {
		return nil, err
	}
	entries := make([]ledger.Entry, 0, len(sealedEntries))
	for _, blob := range sealedEntries {
		e, err := openEntry(blob, priv, pub)
		if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })

	sealedCalls, err := p.Cache.SealedCalls()
	if err != nil {
		return nil, err
	}
	calls := make([]ledger.Call, 0, len(sealedCalls))
	for _, blob := range sealedCalls {
		c, err := openCall(blob, priv, pub)
		if err != nil {
			return nil, err
		}
		calls = append(calls, c)
	}
	sort.Slice(calls, func(i, j int) bool { return calls[i].RequestID < calls[j].RequestID })

	sealedHandoffs, err := p.Cache.SealedHandoffs()
	if err != nil {
		return nil, err
	}
	handoffs := make([]ledger.Handoff, 0, len(sealedHandoffs))
	for _, blob := range sealedHandoffs {
		h, err := openHandoff(blob, priv, pub)
		if err != nil {
			return nil, err
		}
		handoffs = append(handoffs, h)
	}
	sort.Slice(handoffs, func(i, j int) bool { return handoffs[i].ID < handoffs[j].ID })

	// A directly-constructed &Ledger{} (the pattern every unit test uses) has nil
	// diff-tracking maps; its methods tolerate that, and its own Save() — which
	// would diff against those maps — is never called (LedgerStore.Save overrides).
	return &ledger.Ledger{Entries: entries, Calls: calls, Handoffs: handoffs}, nil
}

// Save persists the embedded in-memory ledger to the pocket — the one new
// funds-critical function. It writes the full current state to the NIP-44-sealed
// faithful tables in ONE atomic transaction (PersistLedger), matching the old
// ledger's atomic whole-table Save: a crash mid-save leaves the previous durable
// state intact, never a half (e.g. a source spent with its pending call not yet
// durable). cmd's write-ahead invariant holds because cmd calls AddCall(c) then
// Save() before the wire call, so the pending call is sealed+durable before the
// money moves.
//
// Handoff ids: a freshly-added handoff has id 0; this assigns stable sequential
// ids (as the old Save did) so reloads are stable and a recipient token is never
// lost.
//
// TODO(follow-up: interop projection) — also project held bills to pure-NIP-CP
// 9525s in pocket.db (the universal/syncable copy P5 sync / P6 serve consume).
// Deferred deliberately: a correct projection needs state/rev handling (held →
// close/tombstone) that is non-trivial, and it is NOT needed for cmd correctness
// (the sealed tables above are the source of truth). Until then a migrated
// command's pocket.db is empty; the faithful cache is authoritative.
func (s *LedgerStore) Save() error {
	priv, pub := s.p.userPriv, s.p.userPub
	now := time.Now().Unix()

	var maxHandoffID int64
	for _, h := range s.Handoffs {
		if h.ID > maxHandoffID {
			maxHandoffID = h.ID
		}
	}

	entries := make([]SealedEntry, 0, len(s.Entries))
	for _, e := range s.Entries {
		blob, err := sealEntry(e, priv, pub)
		if err != nil {
			return err
		}
		entries = append(entries, SealedEntry{ID: e.ID, W: e.WalletPubkey, Sealed: blob})
	}

	calls := make([]SealedCall, 0, len(s.Calls))
	for _, c := range s.Calls {
		blob, err := sealCall(c, priv, pub)
		if err != nil {
			return err
		}
		calls = append(calls, SealedCall{RequestID: c.RequestID, Sealed: blob})
	}

	handoffs := make([]SealedHandoff, 0, len(s.Handoffs))
	for i := range s.Handoffs {
		if s.Handoffs[i].ID == 0 {
			maxHandoffID++
			s.Handoffs[i].ID = maxHandoffID // stable id, as the old Save assigned
		}
		blob, err := sealHandoff(s.Handoffs[i], priv, pub)
		if err != nil {
			return err
		}
		handoffs = append(handoffs, SealedHandoff{ID: s.Handoffs[i].ID, Sealed: blob})
	}

	// Staged orders/withdrawals (StageOrder/StageWithdrawal) are flushed in the SAME
	// PersistLedger tx as the calls, so an order/withdrawal and the call that places
	// it are durable together (the old ledger wrote them in one tx; ledger.go:732).
	stagedOrders := make([]SealedOrderRow, 0, len(s.Ledger.StagedOrders()))
	for _, o := range s.Ledger.StagedOrders() {
		blob, err := sealOrder(o, priv, pub)
		if err != nil {
			return err
		}
		stagedOrders = append(stagedOrders, SealedOrderRow{OrderID: o.OrderID, CallID: o.CallID, Sealed: blob})
	}
	stagedWithdrawals := make([]SealedWithdrawalRow, 0, len(s.Ledger.StagedWithdrawals()))
	for _, w := range s.Ledger.StagedWithdrawals() {
		blob, err := sealWithdrawal(w, priv, pub)
		if err != nil {
			return err
		}
		stagedWithdrawals = append(stagedWithdrawals, SealedWithdrawalRow{WithdrawalID: w.WithdrawalID, CallID: w.CallID, Sealed: blob})
	}

	newVersion, err := s.p.Cache.PersistLedger(entries, calls, handoffs, stagedOrders, stagedWithdrawals, now, s.version)
	if err != nil {
		return err
	}
	s.version = newVersion
	// Staged are now durable; clear them (as the old ledger clears after its tx).
	s.Ledger.DropStagedOrders()
	s.Ledger.DropStagedWithdrawals()
	return nil
}

// ---- orders / withdrawals (a separate lazily-loaded funds collection, read &
// written on demand over the sealed orders_full/withdrawals_full tables). These
// OVERRIDE the embedded *ledger.Ledger's cashctl.db-backed methods. The staged
// path (StageOrder/StageWithdrawal) is promoted from the embed and flushed in
// Save (above). encrypt-to-self: the secret fields live inside the sealed blob. ----

func (s *LedgerStore) PutOrder(o ledger.Order) error {
	if o.CreatedAt == 0 {
		o.CreatedAt = time.Now().Unix()
	}
	blob, err := sealOrder(o, s.p.userPriv, s.p.userPub)
	if err != nil {
		return err
	}
	return s.p.Cache.PutSealedOrder(o.OrderID, o.CallID, blob, o.CreatedAt, time.Now().Unix())
}

func (s *LedgerStore) openOrderRow(r SealedOrderRow, ok bool, err error) (ledger.Order, bool, error) {
	if err != nil || !ok {
		return ledger.Order{}, ok, err
	}
	o, err := openOrder(r.Sealed, s.p.userPriv, s.p.userPub)
	return o, err == nil, err
}

func (s *LedgerStore) FindOrder(id string) (ledger.Order, bool, error) {
	r, ok, err := s.p.Cache.SealedOrderByID(id)
	return s.openOrderRow(r, ok, err)
}

func (s *LedgerStore) OrderForCall(callID string) (ledger.Order, bool, error) {
	r, ok, err := s.p.Cache.SealedOrderByCall(callID)
	return s.openOrderRow(r, ok, err)
}

func (s *LedgerStore) Orders() ([]ledger.Order, error) {
	rows, err := s.p.Cache.SealedOrders()
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Order, 0, len(rows))
	for _, r := range rows {
		o, err := openOrder(r.Sealed, s.p.userPriv, s.p.userPub)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

func (s *LedgerStore) DeleteOrder(id string) error { return s.p.Cache.DeleteSealedOrder(id) }

func (s *LedgerStore) PutWithdrawal(w ledger.Withdrawal) error {
	blob, err := sealWithdrawal(w, s.p.userPriv, s.p.userPub)
	if err != nil {
		return err
	}
	return s.p.Cache.PutSealedWithdrawal(w.WithdrawalID, w.CallID, blob, time.Now().Unix())
}

func (s *LedgerStore) openWithdrawalRow(r SealedWithdrawalRow, ok bool, err error) (ledger.Withdrawal, bool, error) {
	if err != nil || !ok {
		return ledger.Withdrawal{}, ok, err
	}
	w, err := openWithdrawal(r.Sealed, s.p.userPriv, s.p.userPub)
	return w, err == nil, err
}

func (s *LedgerStore) FindWithdrawal(id string) (ledger.Withdrawal, bool, error) {
	r, ok, err := s.p.Cache.SealedWithdrawalByID(id)
	return s.openWithdrawalRow(r, ok, err)
}

func (s *LedgerStore) WithdrawalForCall(callID string) (ledger.Withdrawal, bool, error) {
	r, ok, err := s.p.Cache.SealedWithdrawalByCall(callID)
	return s.openWithdrawalRow(r, ok, err)
}

func (s *LedgerStore) Withdrawals() ([]ledger.Withdrawal, error) {
	rows, err := s.p.Cache.SealedWithdrawals()
	if err != nil {
		return nil, err
	}
	out := make([]ledger.Withdrawal, 0, len(rows))
	for _, r := range rows {
		w, err := openWithdrawal(r.Sealed, s.p.userPriv, s.p.userPub)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// ---- derived-id gate (cross-process re-placement lock) over pocket-cache.db's
// derived_ids table (stage 3a-i), overriding the embedded ledger's cashctl.db one.
// INSERT OR IGNORE + RowsAffected is the lock and is driver-agnostic: a second
// insert of the same id affects no row = the "already recorded" answer the old
// ledger gives via ErrDerivedRecorded. LOCAL_ONLY precious state (risk 19). ----

func (s *LedgerStore) InsertDerived(requestID, parent string, deadline *int64, keepUntil int64) error {
	var d any
	if deadline != nil {
		d = *deadline
	}
	res, err := s.p.Cache.db.Exec(
		`INSERT OR IGNORE INTO derived_ids(id, parent_request_id, deadline, keep_until) VALUES(?,?,?,?)`,
		requestID, parent, d, keepUntil)
	if err != nil {
		return fmt.Errorf("pocket ledgerstore: insert derived %s: %w", requestID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ledger.ErrDerivedRecorded // already recorded: another run placed it
	}
	return nil
}

func (s *LedgerStore) RemoveDerived(requestID string) error {
	_, err := s.p.Cache.db.Exec(`DELETE FROM derived_ids WHERE id = ?`, requestID)
	return err
}

func (s *LedgerStore) DerivedParent(requestID string) (string, error) {
	var parent string
	err := s.p.Cache.db.QueryRow(`SELECT parent_request_id FROM derived_ids WHERE id = ?`, requestID).Scan(&parent)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return parent, err
}

func (s *LedgerStore) DerivedRecorded(requestID string) (bool, error) {
	var one int
	err := s.p.Cache.db.QueryRow(`SELECT 1 FROM derived_ids WHERE id = ?`, requestID).Scan(&one)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("pocket ledgerstore: derived recorded %s: %w", requestID, err)
	default:
		return true, nil
	}
}

// VerifyWritten is the store-aware write-ahead durability read-back: re-read the
// call FRESH from the sealed pending_calls table (a true on-disk read, not the
// in-memory buffer) and apply the identical compare the cashctl.db path does
// (ledger.VerifyCallReadBack). The guarantee a bill-creating call is durable in
// pocket-cache.db before it is sent — the pocket equivalent of
// ledger.VerifyWritten's fresh-connection read.
func (s *LedgerStore) VerifyWritten(want ledger.Call) error {
	sealed, err := s.p.Cache.SealedCalls()
	if err != nil {
		return err
	}
	blob, ok := sealed[want.RequestID]
	if !ok {
		return fmt.Errorf("ledger: call %s was not written", want.CallID)
	}
	got, err := openCall(blob, s.p.userPriv, s.p.userPub)
	if err != nil {
		return err
	}
	return ledger.VerifyCallReadBack(got, want)
}
