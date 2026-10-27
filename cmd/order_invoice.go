package cmd

import (
	"errors"
	"fmt"
	"strings"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nipCO"
)

// An order's Lightning invoice is checked here before it is shown to pay
// (plan WP-19, validateOrder): its amount is exactly pay_millis (never an
// amountless invoice, nipCO.CheckOrderInvoice), it expires no later than
// pay_by, and its currency is the Hub's chain.

// invoiceDefaultExpiry is BOLT11's expiry when the invoice names none.
const invoiceDefaultExpiry = 3600

// invoiceCurrencies are the BOLT11 currency prefixes of each chain's
// networks. An invoice with a prefix no row names is refused.
var invoiceCurrencies = map[string][]string{
	"bitcoin":   {"bc", "tb", "tbs", "bcrt", "sb"},
	"flokicoin": {"fc", "tfc", "fcrt", "tf"},
}

// invoiceTimes decodes an invoice's creation time and expiry.
func invoiceTimes(invoice string) (currency string, createdAt, expiry int64, err error) {
	hrp, words, err := bech32.DecodeNoLimit(strings.ToLower(invoice))
	if err != nil {
		return "", 0, 0, fmt.Errorf("invoice: %w", err)
	}
	if !strings.HasPrefix(hrp, "ln") {
		return "", 0, 0, errors.New("invoice: not a Lightning invoice")
	}
	rest := hrp[2:]
	i := strings.IndexAny(rest, "0123456789")
	if i < 0 {
		i = len(rest)
	}
	currency = rest[:i]
	const tsWords, sigWords = 7, 104
	if len(words) < tsWords+sigWords {
		return "", 0, 0, errors.New("invoice: too short")
	}
	for _, w := range words[:tsWords] {
		createdAt = createdAt<<5 | int64(w)
	}
	expiry = invoiceDefaultExpiry
	fields := words[tsWords : len(words)-sigWords]
	for j := 0; j+3 <= len(fields); {
		typ, n := fields[j], int(fields[j+1])*32+int(fields[j+2])
		j += 3
		if j+n > len(fields) {
			return "", 0, 0, errors.New("invoice: truncated field")
		}
		if typ == 6 {
			expiry = 0
			for _, w := range fields[j : j+n] {
				expiry = expiry<<5 | int64(w)
			}
		}
		j += n
	}
	return currency, createdAt, expiry, nil
}

// checkOrderInvoice checks a Lightning order's invoice against the order and
// the Hub's chain.
func checkOrderInvoice(order *nipCO.Order, chain string) error {
	if order.Payment != nipCO.PaymentLightning {
		return nil
	}
	if err := nipCO.CheckOrderInvoice(order); err != nil {
		return err
	}
	currency, createdAt, expiry, err := invoiceTimes(order.Invoice)
	if err != nil {
		return err
	}
	if createdAt+expiry > order.PayBy {
		return fmt.Errorf("the invoice expires after pay_by (%d > %d)", createdAt+expiry, order.PayBy)
	}
	// The user is told to pay by pay_by, so the invoice must last that long
	// (NIP-CO §Lightning: it expires at pay_by).
	if createdAt+expiry < order.PayBy-invoicePayByTolerance {
		return fmt.Errorf("the invoice expires well before pay_by (%d < %d)", createdAt+expiry, order.PayBy)
	}
	for c, prefixes := range invoiceCurrencies {
		for _, p := range prefixes {
			if p == currency {
				if c != chain {
					return fmt.Errorf("the invoice is for %s, the Hub runs on %s", c, chain)
				}
				return nil
			}
		}
	}
	return fmt.Errorf("the invoice's currency %q is not one cashctl knows", currency)
}

// invoicePayByTolerance is how much earlier than pay_by an order's invoice may
// expire (clock skew between the Hub and its node).
const invoicePayByTolerance = 120
