// Package cashkitimport maps a verified cashkit backup (internal/cashkitbackup)
// onto cashctl ledger entries, so `cashctl wallet import-cashkit` can restore a
// cashkit wallet's cash. It turns each live, cash-mode holding into a
// ledger.Entry with its spending secret recovered; it deliberately does not
// import identity-bound (pubkey / connection-key) holdings, whose spend needs
// the cashkit identity key this does not yet carry — those are reported as
// skipped, never silently dropped.
//
// The command layer owns ledger I/O (the empty-wallet check, Add, Save);
// everything here is pure so it can be unit-tested against a real fixture.
package cashkitimport

import (
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/cashkitbackup"
	"github.com/ohstr/cashctl/internal/ledger"
)

// Skip reasons (the keys of Plan.Skipped).
const (
	SkipNotLive     = "not_live"          // spent/expired/redeemed/… — nothing to restore
	SkipPubkey      = "pubkey"            // identity-bound: needs the cashkit identity key
	SkipConnKey     = "connection_key"    // web-identity-bound: needs the cashkit identity key
	SkipNoSecret    = "no_secret"         // cash-mode but no recoverable spending secret
	SkipUndecodable = "undecodable"       // the token itself does not decode
	SkipNonLoki     = "non_loki_currency" // a non-loki bill (e.g. satscash): cashctl's ledger is loki-only
)

// lokiHRP is the token HRP of a loki cash bill (lokicash1…). cashctl's ledger
// carries no currency unit and reports every amount as loki, so only loki bills
// may be imported; a holding of any other currency (satscash, …) is skipped
// rather than folded into the loki total where it would be summed and spent as
// loki (the mix cashkit's own MixedUnitsError exists to prevent).
const lokiHRP = "lokicash"

// Plan is the result of mapping a backup: the cash entries ready to Add, their
// total, and a census of what was skipped and why.
type Plan struct {
	WalletID    string
	Entries     []ledger.Entry
	TotalMillis uint64
	Skipped     map[string]int
}

// BuildPlan reads the backup's holdings and maps every live, cash-mode holding
// to a ledger.Entry (secrets recovered). Entries carry no ID/Status yet — the
// caller Adds them (which stamps both). It returns an error only if the backup
// is structurally unreadable; a holding it cannot import is skipped, not fatal.
func BuildPlan(b *cashkitbackup.Backup) (Plan, error) {
	holdings, err := b.Holdings()
	if err != nil {
		return Plan{}, err
	}
	p := Plan{WalletID: b.WalletID, Skipped: map[string]int{}}
	for _, h := range holdings {
		e, reason, ok := mapHolding(h, b.OpenString)
		if !ok {
			p.Skipped[reason]++
			continue
		}
		if e.AmountMillis != nil {
			p.TotalMillis += *e.AmountMillis
		}
		p.Entries = append(p.Entries, e)
	}
	return p, nil
}

// mapHolding maps one backup holding to a ledger entry, or returns the reason it
// cannot be imported (ok=false). open decrypts a sealed value (b.OpenString in
// production); it is a parameter so the mapping is pure and unit-testable without
// a real encrypted backup.
func mapHolding(h cashkitbackup.Holding, open func(cashkitbackup.Sealed) (string, error)) (ledger.Entry, string, bool) {
	if !ledger.Live(h.Status) {
		return ledger.Entry{}, SkipNotLive, false
	}
	switch h.Mode {
	case "cash":
		// handled below
	case "connection_key":
		return ledger.Entry{}, SkipConnKey, false
	default: // "pubkey" and anything else identity-bound
		return ledger.Entry{}, SkipPubkey, false
	}
	if h.CashSecret == nil {
		return ledger.Entry{}, SkipNoSecret, false
	}
	cs, err := open(*h.CashSecret)
	if err != nil || cs == "" {
		return ledger.Entry{}, SkipNoSecret, false
	}
	tok, err := nipcash.Decode(h.Token)
	if err != nil {
		return ledger.Entry{}, SkipUndecodable, false
	}
	// cashctl's ledger carries no unit, so a non-loki bill (satscash, …) must not
	// be imported — folded into the loki total it would be summed and spent as
	// loki (the cross-currency mix cashkit's MixedUnitsError forbids). nipcash.Decode
	// accepts any HRP, so this is the only guard.
	if tok.HRP != lokiHRP {
		return ledger.Entry{}, SkipNonLoki, false
	}

	idReq := false
	e := ledger.Entry{
		Token:            h.Token,
		WalletPubkey:     h.WalletPubkey,
		Secret:           tok.Secret, // the token's NWC connection secret (type-2 TLV)
		CashSecret:       cs,         // the cash-mode spending credential, recovered from the backup
		RelayURLs:        tok.RelayURLs,
		IdentityRequired: &idReq,
		Verified:         h.Verified,
		ExpiresAt:        h.ExpiresAt,
	}
	if amt, ok := h.Amount.Uint64(); ok {
		a := amt
		e.AmountMillis = &a
	}
	if h.MinterPubkey != "" {
		m := h.MinterPubkey
		e.MinterPubkey = &m
	}
	if h.ThrowawayKey != nil {
		if tk, err := open(*h.ThrowawayKey); err == nil && tk != "" {
			e.ThrowawayKey = tk
		}
	}
	// A protected cashkit holding was re-keyed to a secret only it knows; a shared
	// one still carries the secret as received. Carry that across so cashctl reports
	// the same shared/protected state (and the same "anyone shown this can spend it"
	// warning for a shared bill).
	if h.Protection == "protected" {
		e.CashProtection = ledger.CashProtected
	} else {
		e.CashProtection = ledger.CashShared
	}
	return e, "", true
}

// IdentityBoundSkipped is the number of holdings skipped because they are bound
// to an identity key this importer does not carry yet (pubkey + connection-key).
func (p Plan) IdentityBoundSkipped() int {
	return p.Skipped[SkipPubkey] + p.Skipped[SkipConnKey]
}
