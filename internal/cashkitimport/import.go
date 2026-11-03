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
	SkipNotLive     = "not_live"       // spent/expired/redeemed/… — nothing to restore
	SkipPubkey      = "pubkey"         // identity-bound: needs the cashkit identity key
	SkipConnKey     = "connection_key" // web-identity-bound: needs the cashkit identity key
	SkipNoSecret    = "no_secret"      // cash-mode but no recoverable spending secret
	SkipUndecodable = "undecodable"    // the token itself does not decode
)

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
		if !ledger.Live(h.Status) {
			p.Skipped[SkipNotLive]++
			continue
		}
		switch h.Mode {
		case "cash":
			// handled below
		case "connection_key":
			p.Skipped[SkipConnKey]++
			continue
		default: // "pubkey" and anything else identity-bound
			p.Skipped[SkipPubkey]++
			continue
		}
		if h.CashSecret == nil {
			p.Skipped[SkipNoSecret]++
			continue
		}
		cs, err := b.OpenString(*h.CashSecret)
		if err != nil || cs == "" {
			p.Skipped[SkipNoSecret]++
			continue
		}
		tok, err := nipcash.Decode(h.Token)
		if err != nil {
			p.Skipped[SkipUndecodable]++
			continue
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
			p.TotalMillis += amt
		}
		if h.MinterPubkey != "" {
			m := h.MinterPubkey
			e.MinterPubkey = &m
		}
		if h.ThrowawayKey != nil {
			if tk, err := b.OpenString(*h.ThrowawayKey); err == nil && tk != "" {
				e.ThrowawayKey = tk
			}
		}
		// A protected cashkit holding was re-keyed to a secret only it knows;
		// a shared one still carries the secret as received. Carry that across
		// so cashctl reports the same shared/protected state (and the same
		// "anyone shown this can spend it" warning for a shared bill).
		if h.Protection == "protected" {
			e.CashProtection = ledger.CashProtected
		} else {
			e.CashProtection = ledger.CashShared
		}
		p.Entries = append(p.Entries, e)
	}
	return p, nil
}

// IdentityBoundSkipped is the number of holdings skipped because they are bound
// to an identity key this importer does not carry yet (pubkey + connection-key).
func (p Plan) IdentityBoundSkipped() int {
	return p.Skipped[SkipPubkey] + p.Skipped[SkipConnKey]
}
