package cmd

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// A redeem whose payment may still settle (plan §10.3 "Redeem outcome"). A Hub
// answers INTERNAL for a payment that has begun and may still settle; an
// omission, an incomplete reply or a timeout says nothing either. None of them
// is resent: the bill's own row is read once (cash_status), and
// nipcash.RedeemProgressOf decides. Unclaimed: nothing was paid, the bill is
// live. A tombstone: spent. Claimed: the bill is `redeeming` — counted in no
// balance, offered to no operation, and read again by every `cash status` and
// `cash settle` until the row is unclaimed again (live), the bill is gone
// (spent), the --into wallet reports the invoice settled (redeemed), or the row
// is still claimed 24 h past the invoice's expiry (redeemed, presumed paid,
// re-opened if a later read shows it unclaimed).

// reasonRedeemInFlight is the --json reason of a redeem that may still settle.
const reasonRedeemInFlight = "redeem_in_flight"

// presumedPaidAfter is how long past its invoice's expiry a still-claimed
// redeem is presumed paid.
const presumedPaidAfter = 24 * 3600

// redeemProgressFn reads e's own row once. Swapped in tests.
var redeemProgressFn = readRedeemProgress

// readRedeemProgress is one cash_status read of e's own row.
func readRedeemProgress(ctx context.Context, cmd *cobra.Command, e *ledger.Entry) (nipcash.RedeemProgress, *nipcash.Tombstone, error) {
	cred, err := resolveCredential(cmd, e)
	if err != nil {
		return 0, nil, err
	}
	client, err := dialBill(ctx, e.Token)
	if err != nil {
		return 0, nil, err
	}
	defer client.Close()
	res, err := client.CashStatus(ctx, cred, nipcash.ScopeMine)
	var gone *nipcash.BillGone
	switch {
	case errors.As(err, &gone):
		return nipcash.RedeemDone, &gone.Tombstone, nil
	case err != nil:
		return 0, nil, err
	case res == nil:
		return 0, nil, errors.New("cash_status returned no result")
	case res.Tombstone != nil:
		return nipcash.RedeemDone, res.Tombstone, nil
	}
	row, err := nipcash.OwnRow(res.Recipients, cred, nipcash.ScopeMine)
	if err != nil {
		return 0, nil, err
	}
	return nipcash.RedeemProgressOf(&row, nil), nil, nil
}

// invoiceSettledFn asks the --into wallet whether it was paid the invoice with
// paymentHash. Swapped in tests.
var invoiceSettledFn = func(ctx context.Context, cmd *cobra.Command, into, paymentHash string) (bool, error) {
	_, value, err := resolveDestWallet(cmd, into)
	if err != nil {
		return false, err
	}
	c, err := DialGeneric(ctx, value)
	if err != nil {
		return false, err
	}
	defer c.Close()
	tx, err := c.LookupInvoice(ctx, nip47.LookupInvoiceParams{PaymentHash: paymentHash})
	if err != nil {
		return false, err
	}
	return tx != nil && (tx.SettledAt != nil || tx.State == "settled"), nil
}

// errRedeemInFlight is exit 8 for a bill whose redeem may still settle.
func errRedeemInFlight(cmd *cobra.Command, id string) error {
	return output.WithReason(output.IndeterminateError(cmd, id, fmt.Errorf(
		"the redeem of %s may still settle, so it is held back from everything else: `cashctl cash status` shows the outcome", id)),
		reasonRedeemInFlight)
}

// markRedeeming records e as redeeming into invoice (into is the --into
// connection, "" for --invoice).
func markRedeeming(l *ledger.Ledger, e *ledger.Entry, invoice, into string) {
	e.Status = ledger.StatusRedeeming
	e.RedeemPaymentHash, _ = bolt11PaymentHash(invoice)
	if exp, err := bolt11ExpiresAt(invoice); err == nil {
		e.RedeemInvoiceExpiresAt = &exp
	} else {
		e.RedeemInvoiceExpiresAt = nil
	}
	e.RedeemInto = into
	e.RedeemPresumedAt = nil
	l.AppendHistory("redeem", fmt.Sprintf("the redeem of %s may still settle; held back until it does", e.ID))
}

// clearRedeem drops what a redeem left on e.
func clearRedeem(e *ledger.Entry) {
	e.RedeemPaymentHash, e.RedeemInvoiceExpiresAt, e.RedeemInto, e.RedeemPresumedAt = "", nil, "", nil
}

// endRedeemGone records e's bill destroyed: redeemed when the tombstone says
// so, spent otherwise.
func endRedeemGone(l *ledger.Ledger, e *ledger.Entry, t *nipcash.Tombstone) {
	clearRedeem(e)
	if t != nil && t.Reason == nipcash.ReasonRedeemed {
		e.Status = ledger.StatusRedeemed
		l.AppendHistory("redeem", fmt.Sprintf("the redeem of %s settled: the Hub reports it redeemed", e.ID))
		return
	}
	e.Status = ledger.StatusSpent
	l.AppendHistory("spent", fmt.Sprintf("the Hub reported token %s spent (%s)", e.ID, tombstoneReason(t)))
}

// resolveUndecidedRedeem decides a redeem the Hub's answer left open: coded is
// the error the answer gave (INTERNAL), nil for an omission or no answer. It
// returns the bill's outcome error.
func resolveUndecidedRedeem(ctx context.Context, cmd *cobra.Command, l *ledger.Ledger, e *ledger.Entry, invoice, into string, coded error) error {
	prog, tomb, err := redeemProgressFn(ctx, cmd, e)
	switch {
	case err != nil:
		// The one read failed: nothing says the payment stopped, so the bill
		// is held back and read again later.
		markRedeeming(l, e, invoice, into)
		return errRedeemInFlight(cmd, e.ID)
	case prog == nipcash.RedeemDone:
		endRedeemGone(l, e, tomb)
		if e.Status == ledger.StatusRedeemed {
			return output.SpentError(cmd, e.ID, errors.New(
				"the Hub reports this bill redeemed: the payment went through, but its receipt never reached cashctl — your wallet shows it"))
		}
		return output.SpentError(cmd, e.ID, fmt.Errorf("%w (%s)", errBillSpent, tombstoneReason(tomb)))
	case prog == nipcash.RedeemInFlight:
		markRedeeming(l, e, invoice, into)
		return errRedeemInFlight(cmd, e.ID)
	}
	// Unclaimed: nothing was paid; the bill stays held.
	if coded != nil {
		return coded
	}
	return output.NetworkError(cmd, fmt.Errorf("the Hub did not answer this redeem, and the token is still unclaimed: nothing was paid. Redeem it again with a fresh invoice"))
}

// redeemingSettle is what settleRedeeming reports about one bill.
type redeemingSettle struct {
	ID     string `json:"token"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// settleRedeeming reads every redeeming bill (and every one presumed paid)
// again and ends what it can. Returns what changed and how many remain
// redeeming.
func settleRedeeming(cmd *cobra.Command, l *ledger.Ledger) (out []redeemingSettle, still int) {
	now := time.Now().Unix()
	changed := false
	for i := range l.Entries {
		e := &l.Entries[i]
		presumed := e.Status == ledger.StatusRedeemed && e.RedeemPresumedAt != nil
		if e.Status != ledger.StatusRedeeming && !presumed {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		prog, tomb, err := redeemProgressFn(ctx, cmd, e)
		switch {
		case err != nil:
			cancel()
			if !presumed {
				still++
				out = append(out, redeemingSettle{ID: e.ID, Status: e.Status, Detail: "may still settle; the Hub could not be asked"})
			}
			continue
		case prog == nipcash.RedeemDone:
			endRedeemGone(l, e, tomb)
			out = append(out, redeemingSettle{ID: e.ID, Status: e.Status, Detail: "the Hub reports it " + tombstoneReason(tomb)})
		case prog == nipcash.RedeemReleased:
			clearRedeem(e)
			e.Status = ledger.StatusHeld
			detail := "the payment failed; redeem again with a fresh invoice"
			if presumed {
				detail = "presumed paid, but the Hub shows it unclaimed: it is yours again; redeem again with a fresh invoice"
			}
			l.AppendHistory("redeem", fmt.Sprintf("the redeem of %s did not pay: %s", e.ID, detail))
			out = append(out, redeemingSettle{ID: e.ID, Status: e.Status, Detail: detail})
		case presumed:
			// Still claimed: stays presumed paid.
		case e.RedeemInto != "" && e.RedeemPaymentHash != "" && invoiceSettled(ctx, cmd, e):
			clearRedeem(e)
			e.Status = ledger.StatusRedeemed
			l.AppendHistory("redeem", fmt.Sprintf("the redeem of %s settled: %s reports the invoice paid", e.ID, e.RedeemInto))
			out = append(out, redeemingSettle{ID: e.ID, Status: e.Status, Detail: "your wallet reports the invoice paid"})
		case e.RedeemInvoiceExpiresAt != nil && now > *e.RedeemInvoiceExpiresAt+presumedPaidAfter:
			e.Status = ledger.StatusRedeemed
			e.RedeemPresumedAt = &now
			l.AppendHistory("redeem", fmt.Sprintf("the redeem of %s is presumed paid: still claimed 24 h after its invoice expired", e.ID))
			out = append(out, redeemingSettle{ID: e.ID, Status: e.Status, Detail: "presumed paid: still claimed 24 h after its invoice expired"})
		default:
			still++
			out = append(out, redeemingSettle{ID: e.ID, Status: e.Status, Detail: "may still settle"})
		}
		cancel()
		changed = true
	}
	if changed {
		_ = l.Save()
	}
	return out, still
}

func invoiceSettled(ctx context.Context, cmd *cobra.Command, e *ledger.Entry) bool {
	ok, err := invoiceSettledFn(ctx, cmd, e.RedeemInto, e.RedeemPaymentHash)
	return err == nil && ok
}

// redeemingTotal is how much is being redeemed, and in how many bills.
func redeemingTotal(l *ledger.Ledger) (millis uint64, n int) {
	for _, e := range l.Entries {
		if e.Status != ledger.StatusRedeeming {
			continue
		}
		n++
		if e.AmountMillis != nil {
			millis += *e.AmountMillis
		}
	}
	return millis, n
}
