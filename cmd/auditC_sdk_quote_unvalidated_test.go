package cmd

// Audit C, SDK-abstraction role, surface 2: does any hub-supplied value reach
// cashctl's ledger unvalidated?
//
// FINDING C-SDK-2 (HIGH, local state corruption + silent loss) — CONFIRMED AND FIXED.
//
// `validateClaimedAmount` (cmd/cash_receive.go) was the guard, and its own doc comment
// states the threat model exactly: "a fake Hub can make receive display and PERSIST an
// arbitrary 'verified' balance". It was called from ONE site, receive. The two REDEEM
// quote paths persisted the same hub-supplied number, and set Verified = true, without
// it:
//
//	cmd/redeem_private.go  recordQuote        (the batched path)
//	cmd/cash_redeem.go     resolveRedeemQuote (the single-bill path)
//
// Nothing privileged is needed to reach them: a token's own RelayURLs are dialed
// automatically, so whoever controls the relay answers cash_status.
//
// The fix is validateQuotedAmounts, called from both, before the ledger sees anything.
// It cannot be validateClaimedAmount itself: that one demands EXACT equality with the
// signed provenance, which is right at receive time and wrong for a redeem quote,
// because a slice of a multi-recipient bill is legitimately smaller than the amount the
// mint signature commits to. So the ceiling carries over and the floor cannot — and the
// direction the ceiling cannot refuse (the Hub UNDER-reporting, keeping the difference)
// is handled by making it visible in the confirmation prompt instead of silent.
//
// This file is the ATTACK-PATTERN MATRIX. Every refusal row asserts two things: the
// quote was refused, AND the ledger was left untouched — because persisting first and
// validating after would leave the poison behind on the refusal path too.

import (
	"bytes"
	"encoding/hex"
	"math"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func auditCLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	tmp := t.TempDir()
	appdir.SetOverride(tmp)
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatalf("ledger.Load() error = %v", err)
	}
	return l
}

// auditCQuoteAttack is one row of the matrix: what the Hub answers cash_status with,
// and whether cashctl may act on it.
type auditCQuoteAttack struct {
	name string
	// what the Hub reports for this holder's slice
	amount, fee, net uint64
	// the bill's signed denomination, and whether its signature actually verified
	attested           uint64
	provenanceVerified bool
	// wantRefused: the quote must not be acted on or persisted at all
	wantRefused bool
	// wantDisclosed: accepted, but the confirmation prompt must name both figures
	wantDisclosed bool
	why           string
}

func auditCQuoteMatrix() []auditCQuoteAttack {
	const attested = uint64(1_000_000)
	overflow := uint64(1)<<63 + 1 // one past math.MaxInt64

	return []auditCQuoteAttack{
		// ---- the overflow family: one figure poisons the WHOLE wallet's total ----
		{
			name: "amount_past_maxint64", amount: overflow, fee: 0, net: overflow,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "goes negative at every int64 cast, so `wallet balance` reports a negative total for every held bill",
		},
		{
			name: "fee_past_maxint64", amount: overflow, fee: overflow, net: 0,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "FormatAmount(int64(fee)) in the confirmation prompt",
		},
		{
			name: "net_past_maxint64", amount: overflow, fee: 0, net: overflow,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "net is what redeemInvoiceAmount asks the destination wallet to invoice for",
		},
		{
			name: "exactly_maxint64_is_allowed", amount: math.MaxInt64, fee: 0, net: math.MaxInt64,
			attested: attested, provenanceVerified: false, wantRefused: false,
			why: "the boundary itself is representable; refusing it would move the bug rather than fix it",
		},

		// ---- the arithmetic family: a quote whose own three numbers disagree ----
		{
			name: "net_exceeds_amount", amount: 1000, fee: 0, net: 5000,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "cashctl would mint an invoice for more than the slice holds; lokihub's exact match rejects it after the invoice exists",
		},
		{
			name: "fee_exceeds_amount", amount: 1000, fee: 5000, net: 0,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "lokihub asserts fee <= amount by construction, so this cannot come from an honest Hub",
		},
		{
			name: "net_plus_fee_under_amount_skim", amount: 1000, fee: 100, net: 800,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "100 mloki unaccounted for; net is defined as a plain subtraction, so the gap is the Hub's",
		},
		{
			name: "net_plus_fee_over_amount", amount: 1000, fee: 100, net: 950,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "the same disagreement in the other direction",
		},

		// ---- the provenance family: the signed denomination as a ceiling ----
		{
			name: "amount_exceeds_signed_provenance", amount: attested + 1, fee: 0, net: attested + 1,
			attested: attested, provenanceVerified: true, wantRefused: true,
			why: "no slice of a bill can be worth more than the bill; the mint signature commits to the total",
		},
		{
			name: "amount_equals_signed_provenance", amount: attested, fee: 0, net: attested,
			attested: attested, provenanceVerified: true, wantRefused: false,
			why: "the sole-recipient case, and the ceiling is inclusive",
		},
		{
			name: "inflated_but_provenance_unverified", amount: attested * 2, fee: 0, net: attested * 2,
			attested: attested, provenanceVerified: false, wantRefused: false,
			why: "an unverified attestation is attacker-controlled, so it must not be used as a ceiling either; " +
				"maxSaneAmountMillis is the only bound that applies, and this is within it",
		},

		// ---- the deflation family: what the ceiling CANNOT refuse ----
		{
			name: "deflated_with_zero_fee", amount: 1000, fee: 0, net: 1000,
			attested: attested, provenanceVerified: true, wantRefused: false, wantDisclosed: true,
			why: "indistinguishable from a legitimate slice, so it is accepted — but a zero fee used to hide " +
				"the only line the prompt had, which is exactly why a deflating Hub reports one",
		},
		{
			name: "deflated_to_one_millis", amount: 1, fee: 0, net: 1,
			attested: attested, provenanceVerified: true, wantRefused: false, wantDisclosed: true,
			why: "the extreme of the same shape; the user is the only one who can tell it is wrong",
		},
		{
			name: "honest_sole_recipient_quote", amount: attested, fee: 1000, net: attested - 1000,
			attested: attested, provenanceVerified: true, wantRefused: false, wantDisclosed: false,
			why: "the control: an honest quote must pass and must not raise a provenance line",
		},
	}
}

// TestAuditC_ValidateQuotedAmounts_Matrix runs the matrix against the validator itself.
func TestAuditC_ValidateQuotedAmounts_Matrix(t *testing.T) {
	for _, a := range auditCQuoteMatrix() {
		t.Run(a.name, func(t *testing.T) {
			attested := a.attested
			tok := nipcash.Token{AttestedAmountMillis: &attested}
			var minter *string
			if a.provenanceVerified {
				m := "02" + strings.Repeat("ab", 32)
				minter = &m
			}
			q := redeemQuote{AmountMillis: a.amount, RedeemFeeMillis: a.fee, NetRedeemableMillis: a.net}
			if a.provenanceVerified {
				q.AttestedAmountMillis = &attested
			}

			err := validateQuotedAmounts(testCmdWithFlags(true, false), tok, minter, q)
			switch {
			case a.wantRefused && err == nil:
				t.Fatalf("accepted a quote that must be refused (%s)", a.why)
			case !a.wantRefused && err != nil:
				t.Fatalf("refused a legitimate quote: %v (%s)", err, a.why)
			}

			if !a.wantRefused {
				// The disclosure half: what the user is shown about an accepted quote.
				suffix := previewSuffix(q)
				mentionsProvenance := strings.Contains(suffix, "signed provenance")
				if a.wantDisclosed && !mentionsProvenance {
					t.Errorf("previewSuffix = %q, want it to name the signed total — %s", suffix, a.why)
				}
				if !a.wantDisclosed && mentionsProvenance {
					t.Errorf("previewSuffix = %q, want no provenance line for an honest quote", suffix)
				}
			}
		})
	}
}

// TestAuditC_RecordQuote_Matrix runs the same matrix through the batched path, and adds
// the assertion the validator alone cannot make: a refused quote must leave the ledger
// exactly as it was. Persisting first and validating after would still corrupt local
// state on every refusal.
func TestAuditC_RecordQuote_Matrix(t *testing.T) {
	for _, a := range auditCQuoteMatrix() {
		t.Run(a.name, func(t *testing.T) {
			l := auditCLedger(t)

			// A second, honest bill, present in every row: the overflow rows poison the
			// wallet TOTAL, so the damage was never confined to the bill it arrived on.
			bystanderAmount := uint64(5000)
			bystander, err := l.Add(ledger.Entry{Token: "lokicash1auditcbystander", AmountMillis: &bystanderAmount})
			if err != nil {
				t.Fatalf("Add() error = %v", err)
			}

			// A real token, so recordQuote's own decode finds real signed provenance
			// rather than being handed a struct by the test.
			token, attested, minter := auditCMintedToken(t, a.attested)
			before := attested
			entry, err := l.Add(ledger.Entry{Token: token, AmountMillis: &before})
			if err != nil {
				t.Fatalf("Add() error = %v", err)
			}
			if a.provenanceVerified {
				entry.MinterPubkey = &minter
			}

			q, err := recordQuote(testCmdWithFlags(true, false), l, entry, nipcash.RecipientStatus{
				AmountMillis:        a.amount,
				RedeemFeeMillis:     a.fee,
				NetRedeemableMillis: a.net,
			})

			if a.wantRefused {
				if err == nil {
					t.Fatalf("recordQuote accepted a quote that must be refused (%s)", a.why)
				}
				// The ledger must be untouched: amount unchanged, and NOT marked
				// verified off a figure that was rejected.
				if entry.AmountMillis == nil || *entry.AmountMillis != before {
					t.Errorf("entry amount = %v, want it left at %d — a refused quote must not be persisted",
						entry.AmountMillis, before)
				}
				if entry.Verified {
					t.Error("entry was marked Verified from a quote that was refused")
				}
				// And the wallet total must survive, which is the consequence that
				// made this HIGH rather than cosmetic.
				if saveErr := l.Save(); saveErr != nil {
					t.Fatalf("Save() error = %v", saveErr)
				}
				reloaded, loadErr := ledger.Load()
				if loadErr != nil {
					t.Fatalf("reload error = %v", loadErr)
				}
				_, total, _ := summarizeHeldTokens(reloaded.Held(), 0)
				if total < 0 {
					t.Errorf("wallet total = %d after a refused quote, want it non-negative "+
						"(%s was honest at %d)", total, bystander.ID, bystanderAmount)
				}
				return
			}

			if err != nil {
				t.Fatalf("recordQuote refused a legitimate quote: %v (%s)", err, a.why)
			}
			if q.AmountMillis != a.amount {
				t.Errorf("quote amount = %d, want %d", q.AmountMillis, a.amount)
			}
			if entry.AmountMillis == nil || *entry.AmountMillis != a.amount {
				t.Errorf("entry amount = %v, want the accepted %d", entry.AmountMillis, a.amount)
			}
			if a.provenanceVerified && q.AttestedAmountMillis == nil {
				t.Error("an accepted quote on a verified bill must carry the attested total, " +
					"or previewSuffix cannot disclose a deflation")
			}
			if !a.provenanceVerified && q.AttestedAmountMillis != nil {
				t.Error("an UNVERIFIED attestation must not be presented to the user as signed")
			}
			// The rendered amount must be a real quantity, in every accepted row.
			if rendered := output.FormatAmount(int64(*entry.AmountMillis)); strings.HasPrefix(rendered, "-") {
				t.Errorf("accepted amount renders as %q — an int64 cast went negative", rendered)
			}
		})
	}
}

// auditCMintedToken builds a real encoded token carrying signed provenance for
// attested, so recordQuote's own nipcash.Decode finds it rather than the test handing
// it a struct. Returns the token, the attested amount, and the minter pubkey a caller
// would have recorded on the ledger entry.
//
// The signature bytes are filler, deliberately, and that is the design rather than a
// shortcut: validateQuotedAmounts does not verify the signature, it takes the caller's
// verifiedMinter as proof that nipcash.VerifyProvenance already did — for the reason
// validateClaimedAmount's own doc comment gives, that tok.HasProvenance() means only
// that a signature and an amount are both PRESENT. A test that supplied a real
// signature would be exercising nmilat's verifier, not this guard, and would hide the
// row that matters most: inflated_but_provenance_unverified, where the same attested
// amount must NOT act as a ceiling.
func auditCMintedToken(t *testing.T, attested uint64) (token string, amount uint64, minterPubkey string) {
	t.Helper()
	amount = attested
	tok, err := nipcash.Encode(nipcash.Token{
		HRP:                  "lokicash",
		WalletPubkey:         hex.EncodeToString(bytes.Repeat([]byte{0xab}, 32)),
		Secret:               hex.EncodeToString(bytes.Repeat([]byte{0xcd}, 32)),
		RelayURLs:            []string{"wss://relay.test"},
		MintSignature:        bytes.Repeat([]byte{0x01}, 65),
		AttestedAmountMillis: &amount,
	})
	if err != nil {
		t.Fatalf("nipcash.Encode() error = %v", err)
	}
	return tok, amount, "02" + strings.Repeat("ab", 32)
}
