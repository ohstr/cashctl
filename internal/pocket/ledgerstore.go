package pocket

import (
	"context"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/nmilat/nipCP"
)

// LedgerStore is the pocket-backed implementation of ledger.Store (the seam the
// command layer depends on). It maps between the NIP-CP pocket (the event store
// pocket.db + the decoded cache pocket-cache.db) and the concrete ledger.Entry/
// Call DTOs the cmd layer reads, so a command family can be flipped from the old
// cashctl.db ledger onto the pocket without the cmd layer noticing — stage 2 of
// the pocket-ledger migration (the plan's §8). It is selected only behind a
// construction toggle that defaults off (see cmd.loadStore); until a family is
// migrated (stage 3) nothing constructs it at runtime.
//
// Scope (stage 2): the held-bill reads (Held/Find/FindByToken/AllEntries), Add,
// Save and the call reads map to the pocket. The funds-mutating methods that the
// event-sourced pocket expresses differently — SetStatus/SetVerified (state
// follows from a tombstone/close, not a local write), AddCall/RemoveCall (write-
// ahead is the pocket Journal, not a Store write), AddHandoff (modelled on the
// entry's source/secret), and the order/withdraw staging — are left as loud
// TODO(family-migration) stubs so a premature stage-3 wiring fails obviously
// rather than silently doing the wrong thing. Each such method names the family
// that must wire it.
type LedgerStore struct {
	p   *Pocket
	ctx context.Context
}

var _ ledger.Store = (*LedgerStore)(nil)

// OpenLedgerStore opens the pocket under dir and returns it as a ledger.Store.
// userPriv is the wallet key (the pocket is gift-wrapped to it); chain is the
// default chain stamped on a call that carries none.
func OpenLedgerStore(ctx context.Context, dir, userPriv, chain string) (*LedgerStore, error) {
	p, err := Open(ctx, dir, userPriv, chain)
	if err != nil {
		return nil, err
	}
	return &LedgerStore{p: p, ctx: ctx}, nil
}

// Close releases the underlying pocket (the bbolt lock + the cache).
func (s *LedgerStore) Close() { s.p.Close() }

// ---- the ledger.Entry <-> nipCP.Entry mapping (the heart of the adapter) ----

// ledgerID derives a stable, local "tok-xxxx" handle from a pocket entry's
// wallet pubkey, so Find(id) is consistent across calls. The old ledger's IDs
// were random and per-row; the pocket is keyed by w, so a deterministic handle
// from w gives the cmd layer the same shape with a stable value.
func ledgerID(w string) string {
	if len(w) >= 4 {
		return "tok-" + w[:4]
	}
	return "tok-" + w
}

// statusFromState maps a pocket state to the ledger status the cmd layer reads.
// A pocket entry written from a tombstone/close carries the final state directly;
// cashctl's finer in-flight statuses (redeeming/zap_claiming) are pocket pending
// concerns a family migration refines.
func statusFromState(st nipCP.State) string {
	switch st {
	case nipCP.StateLive:
		return ledger.StatusHeld
	case nipCP.StateSpent:
		return ledger.StatusSpent
	case nipCP.StateExpired:
		return ledger.StatusExpired
	case nipCP.StateReclaimed:
		return ledger.StatusReclaimed
	default:
		// pending/done/unknown have no held-bill status; treat as not-held.
		return ledger.StatusSpent
	}
}

// stateFromStatus is statusFromState's inverse for Add (a newly held bill is
// live). Non-live statuses are not added through this path.
func stateFromStatus(status string) nipCP.State {
	switch status {
	case ledger.StatusSpent, ledger.StatusTransferred, ledger.StatusConsolidated, ledger.StatusWithdrawn:
		return nipCP.StateSpent
	case ledger.StatusExpired:
		return nipCP.StateExpired
	case ledger.StatusReclaimed:
		return nipCP.StateReclaimed
	default:
		return nipCP.StateLive
	}
}

// toLedgerEntry maps a resolved pocket (held-bill) entry to the ledger.Entry DTO.
// Token-derived fields (the NWC secret, relay hints, identity-required) are
// recovered by decoding the token, exactly as the old ledger treats them (cached
// from the token, not a separate source of truth); only the fields the token
// cannot carry (status, identity binding, minter, expiry, cash secret) are mapped
// from the pocket entry.
func (s *LedgerStore) toLedgerEntry(e nipCP.Entry) ledger.Entry {
	amt := e.Amount
	le := ledger.Entry{
		ID:           ledgerID(e.W),
		Token:        e.Cash,
		WalletPubkey: e.W,
		AmountMillis: &amt,
		Status:       statusFromState(e.State),
		ExpiresAt:    e.ExpiresAt,
		Verified:     true, // a resolved pocket entry came from a real, stored wrap
	}
	if e.Mint != "" {
		m := e.Mint
		le.MinterPubkey = &m
	}
	switch e.Identity.Type {
	case nipCP.IdentityPubkey:
		le.IdentityPubkey = e.Identity.Value
	case nipCP.IdentityConnectionKey:
		// The pocket identity carries the connection key + IA, not cashctl's
		// platform/external-id split; a NIP-IC/zap family migration maps those.
		le.IAPubkey = e.Identity.IA
	case nipCP.IdentityCash:
		le.CashProtection = ledger.CashProtected
		if sec := heldCashSecret(e); sec != "" {
			le.CashSecret = sec
		}
	}
	// Best-effort token-derived fields (display convenience; the token is the
	// source of truth). A decode failure leaves them unset, which the cmd layer
	// already tolerates for legacy entries.
	if tok, err := nipcash.Decode(e.Cash); err == nil {
		if tok.IdentityRequired != nil {
			le.IdentityRequired = tok.IdentityRequired
		}
	}
	return le
}

// heldCashSecret returns the cash secret a held cash-mode entry carries (the
// unroled secret), "" if none.
func heldCashSecret(e nipCP.Entry) string {
	for _, sec := range e.Secrets {
		if sec.Role == "" {
			return sec.Secret
		}
	}
	return ""
}

// toPocketEntry maps a ledger.Entry to a pocket entry for Add (a newly held
// bill). The pocket needs w, cash, amount, chain, identity and a live state;
// chain is read from the token's HRP.
func (s *LedgerStore) toPocketEntry(le ledger.Entry) (nipCP.Entry, error) {
	w := le.WalletPubkey
	if w == "" {
		var err error
		if w, err = billform.WalletPubkey(le.Token); err != nil {
			return nipCP.Entry{}, fmt.Errorf("pocket ledgerstore: wallet pubkey from token: %w", err)
		}
	}
	tok, err := nipcash.Decode(le.Token)
	if err != nil {
		return nipCP.Entry{}, fmt.Errorf("pocket ledgerstore: decode token: %w", err)
	}
	chain, ok := nipCZ.ChainForHRP(tok.HRP)
	if !ok {
		return nipCP.Entry{}, fmt.Errorf("pocket ledgerstore: unknown chain for %q", tok.HRP)
	}
	amt := uint64(0)
	if le.AmountMillis != nil {
		amt = *le.AmountMillis
	} else if tok.AttestedAmountMillis != nil {
		amt = *tok.AttestedAmountMillis
	}
	e := nipCP.Entry{
		W:         w,
		Rev:       1,
		Cash:      le.Token,
		Amount:    amt,
		Chain:     chain.Name,
		State:     stateFromStatus(le.Status),
		CheckedAt: time.Now().Unix(),
		ExpiresAt: le.ExpiresAt,
		Identity:  identityOf(le),
	}
	if le.MinterPubkey != nil {
		e.Mint = *le.MinterPubkey
	}
	if le.CashSecret != "" {
		e.Secrets = []nipcash.PendingSecret{{Secret: le.CashSecret}}
	}
	return e, nil
}

// identityOf maps a ledger.Entry's identity binding to a pocket EntryIdentity.
func identityOf(le ledger.Entry) nipCP.EntryIdentity {
	switch {
	case le.IdentityPubkey != "":
		return nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: le.IdentityPubkey}
	case le.CashProtection != "" || le.CashSecret != "":
		return nipCP.EntryIdentity{Type: nipCP.IdentityCash}
	case le.IAPubkey != "":
		// connection-key value isn't on the ledger entry as a single field;
		// a NIP-IC/zap family migration supplies it. Default to cash-less pubkey.
		return nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: le.WalletPubkey}
	default:
		return nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: le.WalletPubkey}
	}
}

// ---- reads (held bills) ----

func (s *LedgerStore) entries() ([]nipCP.Entry, error) {
	es, _, err := s.p.Cache.Entries()
	return es, err
}

// AllEntries returns every resolved bill entry (not the pending-call rows).
func (s *LedgerStore) AllEntries() []ledger.Entry {
	es, err := s.entries()
	if err != nil {
		return nil
	}
	var out []ledger.Entry
	for _, e := range es {
		if e.IsCall() {
			continue
		}
		out = append(out, s.toLedgerEntry(e))
	}
	return out
}

// Held returns every live (StatusHeld) entry.
func (s *LedgerStore) Held() []ledger.Entry {
	var held []ledger.Entry
	for _, le := range s.AllEntries() {
		if le.Status == ledger.StatusHeld {
			held = append(held, le)
		}
	}
	return held
}

// Find looks up a bill by its derived local id.
func (s *LedgerStore) Find(id string) (*ledger.Entry, bool) {
	for _, le := range s.AllEntries() {
		if le.ID == id {
			e := le
			return &e, true
		}
	}
	return nil, false
}

// FindByToken looks up a bill by the token it spells (by wallet pubkey).
func (s *LedgerStore) FindByToken(token string) (*ledger.Entry, bool) {
	w, err := billform.WalletPubkey(token)
	if err != nil {
		return nil, false
	}
	for _, le := range s.AllEntries() {
		if le.WalletPubkey == w {
			e := le
			return &e, true
		}
	}
	return nil, false
}

// Add records a newly held bill into the pocket (sealed into pocket.db + folded
// into the cache).
func (s *LedgerStore) Add(e ledger.Entry) (*ledger.Entry, error) {
	pe, err := s.toPocketEntry(e)
	if err != nil {
		return nil, err
	}
	if err := s.p.Add(s.ctx, pe); err != nil {
		return nil, err
	}
	if le, ok := s.FindByToken(e.Token); ok {
		return le, nil
	}
	out := s.toLedgerEntry(pe)
	return &out, nil
}

// Save is a no-op: pocket writes are immediate and durable (sealed into the
// event store on each Add/Apply), unlike the old ledger's whole-table flush.
func (s *LedgerStore) Save() error { return nil }

// ---- call reads (partial: the lean pending entry; re-placement params live in
// the sealed wrap, re-Opened by the settle-family migration) ----

// AllCalls maps the pocket's pending-call rows to ledger.Call. Only the fields
// the decoded cache projects are populated (id, request id, deadline, target,
// device, amount, chain, state); the richer re-placement params (authorizer,
// source ids, params json, method, purpose) live in the sealed wrap and are
// supplied by the settle-family migration (stage 3).
func (s *LedgerStore) AllCalls() []ledger.Call {
	es, err := s.entries()
	if err != nil {
		return nil
	}
	var out []ledger.Call
	for _, e := range es {
		if !e.IsCall() {
			continue
		}
		out = append(out, toLedgerCall(e))
	}
	return out
}

func toLedgerCall(e nipCP.Entry) ledger.Call {
	state := ledger.CallWritten
	if e.State == nipCP.StateDone {
		state = ledger.CallDone
	}
	return ledger.Call{
		CallID:       ledger.CallIDFor(e.RequestID),
		RequestID:    e.RequestID,
		Deadline:     e.Deadline,
		Target:       e.Target,
		DeviceID:     e.Device,
		AmountMillis: e.Amount,
		Chain:        e.Chain,
		State:        state,
	}
}

// FindCall looks up a pending call by its 12-hex handle.
func (s *LedgerStore) FindCall(callID string) (*ledger.Call, bool) {
	for _, c := range s.AllCalls() {
		if c.CallID == callID {
			cc := c
			return &cc, true
		}
	}
	return nil, false
}

// OpenCallFor returns the open call that spends entryID, if any. In the pocket,
// a pending call is keyed by its own request id, not the source entry; resolving
// "the open call for this bill" needs the re-Opened wrap's source list, so this
// is wired by the settle-family migration. Returns no call for now (never a
// wrong one).
func (s *LedgerStore) OpenCallFor(entryID string) (*ledger.Call, bool) {
	// TODO(family-migration: settle) — map source-entry → open call via the
	// re-Opened pending wrap's source list. Returning "none" is safe (never a
	// wrong call); no command uses the pocket store until stage 3.
	return nil, false
}

// ---- funds-mutating methods the event-sourced pocket expresses differently:
// loud TODO(family-migration) stubs so a premature wiring fails obviously ----

func (s *LedgerStore) SetStatus(id, status string) error {
	panic("TODO(family-migration): pocket LedgerStore.SetStatus — pocket state follows a tombstone/close, not a local write; the spending family wires the real transition")
}

func (s *LedgerStore) SetVerified(id string, verified bool) error {
	panic("TODO(family-migration): pocket LedgerStore.SetVerified — a resolved pocket entry is already Hub-checked; the receive family wires on-demand re-check")
}

func (s *LedgerStore) AddCall(c ledger.Call) (*ledger.Call, error) {
	panic("TODO(family-migration): pocket LedgerStore.AddCall — write-ahead is the pocket Journal (WriteAhead), not a Store write; the settle family wires it")
}

func (s *LedgerStore) RemoveCall(callID string) bool {
	panic("TODO(family-migration): pocket LedgerStore.RemoveCall — a pocket call is closed by Apply/Lookup, not removed; the settle family wires it")
}

func (s *LedgerStore) AddHandoff(h ledger.Handoff) {
	panic("TODO(family-migration): pocket LedgerStore.AddHandoff — a recipient hand-off is modelled on the entry's source/secret; the transfer/zap family wires it")
}

func (s *LedgerStore) StageOrder(o ledger.Order) {
	panic("TODO(family-migration: order): pocket LedgerStore.StageOrder")
}

func (s *LedgerStore) DropStagedOrders() {
	panic("TODO(family-migration: order): pocket LedgerStore.DropStagedOrders")
}

func (s *LedgerStore) StageWithdrawal(w ledger.Withdrawal) {
	panic("TODO(family-migration: withdraw): pocket LedgerStore.StageWithdrawal")
}

func (s *LedgerStore) DropStagedWithdrawals() {
	panic("TODO(family-migration: withdraw): pocket LedgerStore.DropStagedWithdrawals")
}

// ---- display-only reads the pocket does not model yet: documented empties
// (not a wrong answer — the pocket genuinely has no local action log / handoff
// table; a family migration decides whether to persist them) ----

// AppendHistory is a no-op: cashctl's local action log is display-only and not
// part of the NIP-CP pocket. A family migration may persist history in the cache
// meta if it proves worth keeping.
func (s *LedgerStore) AppendHistory(action, detail string) {}

// AllHistory returns no history: the pocket does not keep cashctl's local action
// log (see AppendHistory).
func (s *LedgerStore) AllHistory() []ledger.HistoryEntry { return nil }

// AllHandoffs returns no handoffs: hand-offs are modelled on the pocket entry's
// source/secret, not a separate table (see AddHandoff).
func (s *LedgerStore) AllHandoffs() []ledger.Handoff { return nil }
