package cmd

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
)

// validateInvoiceShape is a local, no-network sanity check that invoice
// could be a BOLT11 invoice at all: valid bech32 (checksum included) with a
// human-readable part starting "ln" (lnbc, lntb, lnfc, ...). It deliberately
// stops there — decoding the description/signature is the wallet's job —
// but that's enough to catch a typo or a mis-paste up front. Without it a
// malformed invoice was only discovered after the confirm prompt, when the
// wallet rejected it, and came back classified as a retryable `network`
// failure for input that can never work.
func validateInvoiceShape(invoice string) error {
	hrp, _, err := bech32.DecodeNoLimit(strings.TrimSpace(invoice))
	if err != nil || !strings.HasPrefix(strings.ToLower(hrp), "ln") {
		return fmt.Errorf("not a valid Lightning invoice")
	}
	return nil
}

// bolt11AmountMloki reads ONLY the amount a BOLT11 invoice encodes in its
// own human-readable part (never the description/payee/signature — those
// stay the wallet's job, same boundary validateInvoiceShape draws) so
// `pay` can show what it's about to spend before confirming, instead of
// asking a human to confirm a bare, unreadable invoice string. Returns nil
// (not an error) for a valid amountless invoice — "pay whatever you like"
// is legal BOLT11, not a malformed one.
//
// Per BOLT11: after the "ln" + chain prefix (lowercase letters — "bc",
// "tb", "fc", "bcrt", ... — length varies by chain, so this reads until
// the first DIGIT rather than assuming a prefix length; scanning for the
// first non-letter instead would misread a chain prefix ending in a
// multiplier-shaped letter, e.g. hypothetical "lnbcm" with no amount at
// all, as a malformed one-character amount), an optional amount is a
// decimal digit string with an optional trailing multiplier letter (m/u/n/p
// for milli/micro/nano/pico of one whole coin); no multiplier means whole
// coins. Every case converts to msat — this project's own "mloki" wire
// unit (see nip47's Amount fields; 1 whole coin == 10^11 msat/mloki).
func bolt11AmountMloki(invoice string) (*int64, error) {
	hrp, _, err := bech32.DecodeNoLimit(strings.TrimSpace(invoice))
	if err != nil {
		return nil, fmt.Errorf("not a valid Lightning invoice")
	}
	hrp = strings.ToLower(hrp)
	if !strings.HasPrefix(hrp, "ln") {
		return nil, fmt.Errorf("not a valid Lightning invoice")
	}
	i := 2
	for i < len(hrp) && (hrp[i] < '0' || hrp[i] > '9') {
		i++
	}
	amountPart := hrp[i:]
	if amountPart == "" {
		return nil, nil // amountless invoice — legal, no amount to preview
	}

	// amountPart always starts with a digit (the scan above stops at the
	// first one), so digits can only be empty here if multiplier-stripping
	// somehow consumed the whole thing — never, since that only ever
	// removes the LAST character. Guarded anyway: a malformed amount
	// should fall back to the generic prompt, never panic on an empty
	// ParseUint input.
	digits := amountPart
	var multiplier byte
	if last := amountPart[len(amountPart)-1]; last < '0' || last > '9' {
		multiplier = last
		digits = amountPart[:len(amountPart)-1]
	}
	if digits == "" {
		return nil, fmt.Errorf("invoice amount is malformed")
	}
	value, err := strconv.ParseUint(digits, 10, 63)
	if err != nil {
		return nil, fmt.Errorf("invoice amount is malformed")
	}

	const wholeCoinMloki = 100_000_000_000 // 1 coin == 10^11 msat/mloki
	var scale uint64
	switch multiplier {
	case 0:
		scale = wholeCoinMloki
	case 'm':
		scale = wholeCoinMloki / 1_000
	case 'u':
		scale = wholeCoinMloki / 1_000_000
	case 'n':
		scale = wholeCoinMloki / 1_000_000_000
	case 'p':
		// 10^-12 coin == 0.1 mloki — legal only when it lands on a whole
		// mloki (BOLT11 itself requires this; a fractional-mloki amount
		// isn't representable on the wire).
		if value%10 != 0 {
			return nil, fmt.Errorf("invoice amount is not a whole millisatoshi")
		}
		result := int64(value / 10)
		return &result, nil
	default:
		return nil, fmt.Errorf("invoice amount has an unrecognized unit")
	}
	// Bounded before multiplying, not after — see ParseAmount's own doc
	// comment (internal/output/output.go) for why an unchecked multiply
	// here is exactly the overflow class that used to let a typed amount
	// silently wrap.
	if value > uint64(math.MaxInt64)/scale {
		return nil, fmt.Errorf("invoice amount is too large")
	}
	result := int64(value * scale)
	return &result, nil
}
