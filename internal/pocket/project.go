package pocket

import (
	"context"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/billform"
	"github.com/ohstr/cashctl/internal/ledger"
)

// Project reconciles cashctl's authoritative bills into pocket.db as PURE NIP-CP
// kind-9525 held-token entries — the universal, syncable, serveable surface a
// NIP-CP wallet (e.g. cashkit) reads, and what P5 sync / P6 serve publish.
//
// It is a ONE-WAY, derived projection: pocket-cache.db (the sealed faithful
// ledger) stays authoritative; pocket.db is rebuilt from it. Idempotent: it reads
// the current pocket.db resolution and writes only a NEW or STATE-CHANGED bill, at
// the next rev — so a held→spent transition supersedes by rev, and a re-run is a
// no-op. The entries carry NO cashctl-extension tags (unlike the sealed pending
// calls): they are the plain NIP-CP held-token form.
//
// Project touches the bbolt event store (pocket.db), so it is kept OFF the hot
// ledger path (loadStore opens sqlite only). P5 sync / P6 serve open the full
// Pocket (Open, not OpenLedgerOnly) and call this before publishing.
func (p *Pocket) Project(ctx context.Context, entries []ledger.Entry) error {
	if p.Store == nil {
		return fmt.Errorf("pocket: Project needs the event store (pocket.db) open")
	}
	wraps, err := p.Store.Query(ctx, localRelay, nipCP.FullSyncFilter(p.userPub))
	if err != nil {
		return fmt.Errorf("pocket: project read: %w", err)
	}
	all, err := nipCP.OpenAll(wraps, p.userPriv)
	if err != nil {
		return fmt.Errorf("pocket: project open: %w", err)
	}
	winners, _ := nipCP.Resolve(all)
	for i := range entries {
		want, ok := projectEntry(entries[i])
		if !ok {
			continue
		}
		if cur, has := winners[want.W]; has &&
			cur.State == want.State && cur.Cash == want.Cash && cur.Amount == want.Amount {
			continue // already projected, unchanged — idempotent no-op
		}
		want.Rev = nipCP.NextVersion(all, want.W)
		wrap, sealed, err := nipCP.Seal(want, p.userPriv)
		if err != nil {
			return fmt.Errorf("pocket: project seal %s: %w", want.W, err)
		}
		if _, err := nipCP.Store(ctx, p.Store, p.relays, wrap); err != nil {
			return fmt.Errorf("pocket: project store %s: %w", want.W, err)
		}
		// Keep NextVersion/Resolve consistent for the rest of this batch.
		all = append(all, sealed)
		winners[want.W] = sealed
	}
	return nil
}

// projectEntry maps a ledger.Entry to the PURE NIP-CP held-token entry: w, cash
// token, amount, chain, identity, state, mint, expiry, zap source, and (for a
// cash-mode note) the bearer secret. No cashctl-extension tags — the universal
// form any NIP-CP wallet reads. ok=false when it can't be projected (no wallet
// pubkey resolvable / unknown chain).
func projectEntry(le ledger.Entry) (nipCP.Entry, bool) {
	w := le.WalletPubkey
	if w == "" {
		var err error
		if w, err = billform.WalletPubkey(le.Token); err != nil {
			return nipCP.Entry{}, false
		}
	}
	tok, err := nipcash.Decode(le.Token)
	if err != nil {
		return nipCP.Entry{}, false
	}
	chain, ok := nipCZ.ChainForHRP(tok.HRP)
	if !ok {
		return nipCP.Entry{}, false
	}
	amt := uint64(0)
	switch {
	case le.AmountMillis != nil:
		amt = *le.AmountMillis
	case tok.AttestedAmountMillis != nil:
		amt = *tok.AttestedAmountMillis
	}
	e := nipCP.Entry{
		W:         w,
		Cash:      le.Token,
		Amount:    amt,
		Chain:     chain.Name,
		State:     projectState(le.Status),
		CheckedAt: time.Now().Unix(),
		ExpiresAt: le.ExpiresAt,
		Identity:  projectIdentity(le),
	}
	if le.MinterPubkey != nil {
		e.Mint = *le.MinterPubkey
	}
	if le.ZapEvent != "" {
		// The 9522 provenance of a zap bill (NIP-CP source) — recovers the minter.
		e.Source = le.ZapEvent
	}
	if le.CashSecret != "" {
		// A cash-mode (bearer) note carries its secret in the (gift-wrapped) entry
		// so a synced device can spend it; NIP-CP §Kind 9525 (secret, optional).
		e.Secrets = []nipcash.PendingSecret{{Secret: le.CashSecret}}
	}
	return e, true
}

// projectState maps a cashctl ledger status to the NIP-CP held-token state.
func projectState(status string) nipCP.State {
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

// projectIdentity maps a ledger.Entry's binding to the NIP-CP identity. A
// connection-key bill's full connection-key value isn't a single ledger field, so
// (as the stage-2 adapter did) it defaults to the wallet pubkey — a NIP-IC refine
// is future work; the universal held-token is still readable.
func projectIdentity(le ledger.Entry) nipCP.EntryIdentity {
	switch {
	case le.IdentityPubkey != "":
		return nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: le.IdentityPubkey}
	case le.CashProtection != "" || le.CashSecret != "":
		return nipCP.EntryIdentity{Type: nipCP.IdentityCash}
	default:
		return nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: le.WalletPubkey}
	}
}

// entryFromPocket is the inverse of projectEntry: a pulled PURE NIP-CP held-token
// 9525 → a cashctl ledger.Entry, so a cross-device-synced bill becomes SPENDABLE
// by the money commands (not just visible in the decoded cache). ok=false for
// anything that must NOT be imported as a spendable held bill:
//   - a non-live entry (spent/expired/reclaimed/pending/tombstone);
//   - a pubkey-bound entry NOT bound to THIS user — foreign; the user's key can't
//     sign for it (NIP-CP sync: skip foreign pubkey-bound entries);
//   - a cash-mode (bearer) entry with no secret — unspendable without it;
//   - a connection-key entry (NIP-IC binding is future work).
//
// Verified is taken from the token's own mint-signature provenance, never
// over-claimed; the Hub re-arbitrates on every spend regardless.
func entryFromPocket(e nipCP.Entry, userPub string) (ledger.Entry, bool) {
	if e.State != nipCP.StateLive || e.Cash == "" {
		return ledger.Entry{}, false
	}
	amt := e.Amount
	le := ledger.Entry{
		Token:        e.Cash,
		WalletPubkey: e.W,
		AmountMillis: &amt,
		Status:       ledger.StatusHeld,
		ReceivedAt:   time.Now().UTC().Format(time.RFC3339),
		ExpiresAt:    e.ExpiresAt,
	}
	if e.Mint != "" {
		m := e.Mint
		le.MinterPubkey = &m
	}
	if e.Source != "" {
		le.ZapEvent = e.Source
	}
	switch e.Identity.Type {
	case nipCP.IdentityCash:
		sec := unroledSecret(e)
		if sec == "" {
			return ledger.Entry{}, false // bearer note with no secret — unspendable
		}
		le.CashProtection = ledger.CashProtected
		le.CashSecret = sec
	case nipCP.IdentityPubkey:
		if e.Identity.Value != userPub {
			return ledger.Entry{}, false // foreign-bound — the user's key can't sign for it
		}
		le.IdentityPubkey = e.Identity.Value
	default:
		return ledger.Entry{}, false // connection-key / other: NIP-IC future work
	}
	if tok, err := nipcash.Decode(e.Cash); err == nil {
		_, le.Verified = nipcash.VerifyProvenance(tok)
	}
	return le, true
}

// unroledSecret returns the entry's bearer (unroled) cash secret, "" if none.
func unroledSecret(e nipCP.Entry) string {
	for _, s := range e.Secrets {
		if s.Role == "" {
			return s.Secret
		}
	}
	return ""
}

// ImportHeld reverse-projects pulled held-token 9525s into the AUTHORITATIVE
// sealed ledger so cross-device-synced bills become SPENDABLE by the money
// commands. cash_pocket sync folds pulled entries into the decoded cache (for
// display); this adds the importable held bills to entries_full (for spending).
// It loads the sealed ledger, Adds each importable held bill the device doesn't
// already hold (dedup by wallet pubkey), and persists via the LedgerStore
// (sealing + the store_version CAS + byte-identical reload). One-way and
// idempotent: a re-sync adds nothing new; a bill already held is not duplicated.
// p must be the full Pocket (Open), not OpenLedgerOnly. Returns the count made
// spendable. Spends are still arbitrated by the Hub; this only makes the bill
// visible to token selection.
func (p *Pocket) ImportHeld(ctx context.Context, entries []nipCP.Entry) (int, error) {
	l, err := loadLedger(p)
	if err != nil {
		return 0, err
	}
	version, err := p.Cache.StoreVersion()
	if err != nil {
		return 0, err
	}
	ls := &LedgerStore{Ledger: l, p: p, ctx: ctx, version: version}
	have := make(map[string]bool, len(ls.Entries))
	for i := range ls.Entries {
		if ls.Entries[i].WalletPubkey != "" {
			have[ls.Entries[i].WalletPubkey] = true
		}
	}
	n := 0
	for i := range entries {
		le, ok := entryFromPocket(entries[i], p.userPub)
		if !ok || have[le.WalletPubkey] {
			continue
		}
		if _, err := ls.Add(le); err != nil {
			return n, err
		}
		have[le.WalletPubkey] = true
		n++
	}
	if n > 0 {
		if err := ls.Save(); err != nil {
			return n, err
		}
	}
	return n, nil
}
