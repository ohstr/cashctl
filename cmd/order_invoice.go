package cmd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nipCO"

	"github.com/ohstr/cashctl/internal/ledger"
)

// An order's Lightning invoice is checked here before it is shown to pay
// (plan WP-19, validateOrder): its amount is exactly pay_millis (never an
// amountless invoice, nipCO.CheckOrderInvoice), it expires at pay_by, its
// currency is the Hub's chain and network, and it is signed by the Hub's
// node identity: the node the Hub runs on, which issues its invoices.

// invoiceDefaultExpiry is BOLT11's expiry when the invoice names none.
const invoiceDefaultExpiry = 3600

// invoiceCurrencies are the BOLT11 currency prefixes of each chain's
// networks, as get_info names them. An invoice with a prefix no row names is
// refused; a Hub that names no known network is held to its chain's prefixes.
var invoiceCurrencies = map[string]map[string][]string{
	"bitcoin":   {"mainnet": {"bc"}, "testnet": {"tb"}, "signet": {"tbs"}, "regtest": {"bcrt"}},
	"flokicoin": {"mainnet": {"fc"}, "testnet": {"tf", "tfc"}, "signet": {"tf", "tfc"}, "regtest": {"fcrt"}},
}

// invoiceCurrencyOK reports whether currency is an invoice prefix of chain on
// network (any of chain's networks when network is not one cashctl knows).
func invoiceCurrencyOK(currency, chain, network string) (known bool, ok bool) {
	for c, nets := range invoiceCurrencies {
		for n, prefixes := range nets {
			if slices.Contains(prefixes, currency) {
				known = true
				if c == chain && (n == network || invoiceCurrencies[chain][network] == nil) {
					ok = true
				}
			}
		}
	}
	return known, ok
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
// the Hub's chain, network and node identity.
func checkOrderInvoice(order *nipCO.Order, o *ledger.Order) error {
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
	switch known, ok := invoiceCurrencyOK(currency, o.Chain, o.Network); {
	case !known:
		return fmt.Errorf("the invoice's currency %q is not one cashctl knows", currency)
	case !ok:
		return fmt.Errorf("the invoice's currency %q is not the Hub's chain and network (%s, %s)", currency, o.Chain, o.Network)
	}
	payee, err := invoicePayee(order.Invoice)
	if err != nil {
		return err
	}
	if o.HubIdentity == "" || payee != strings.ToLower(o.HubIdentity) {
		return fmt.Errorf("the invoice pays node %s, not the Hub's node identity %s", payee, o.HubIdentity)
	}
	return nil
}

// invoicePayee is the x-only key of the node a BOLT11 invoice pays: the key its
// signature over sha256(hrp || data) recovers, which must equal an "n" field
// when the invoice carries one.
func invoicePayee(invoice string) (string, error) {
	hrp, words, err := bech32.DecodeNoLimit(strings.ToLower(invoice))
	if err != nil {
		return "", fmt.Errorf("invoice: %w", err)
	}
	const tsWords, sigWords = 7, 104
	if len(words) < tsWords+sigWords {
		return "", errors.New("invoice: too short")
	}
	data, sigPart := words[:len(words)-sigWords], words[len(words)-sigWords:]
	sig, err := bech32.ConvertBits(sigPart, 5, 8, false)
	if err != nil || len(sig) != 65 || sig[64] > 3 {
		return "", errors.New("invoice: bad signature field")
	}
	msg, err := bech32.ConvertBits(data, 5, 8, true)
	if err != nil {
		return "", fmt.Errorf("invoice: %w", err)
	}
	h := sha256.Sum256(append([]byte(hrp), msg...))
	compact := append([]byte{27 + 4 + sig[64]}, sig[:64]...)
	pub, _, err := ecdsa.RecoverCompact(compact, h[:])
	if err != nil {
		return "", fmt.Errorf("invoice: the signature recovers no key: %w", err)
	}
	if n, ok := invoiceNodeField(data[tsWords:]); ok && !bytes.Equal(n, pub.SerializeCompressed()) {
		return "", errors.New("invoice: the signature is not by the node its n field names")
	}
	return hex.EncodeToString(pub.SerializeCompressed()[1:]), nil
}

// invoiceNodeField is a BOLT11 "n" field (type 19, a 33-byte key), if any.
func invoiceNodeField(fields []byte) ([]byte, bool) {
	for j := 0; j+3 <= len(fields); {
		typ, n := fields[j], int(fields[j+1])*32+int(fields[j+2])
		j += 3
		if j+n > len(fields) {
			return nil, false
		}
		if typ == 19 && n == 53 {
			if b, err := bech32.ConvertBits(fields[j:j+n], 5, 8, false); err == nil && len(b) == 33 {
				return b, true
			}
		}
		j += n
	}
	return nil, false
}

// invoicePayByTolerance is how much earlier than pay_by an order's invoice may
// expire (clock skew between the Hub and its node).
const invoicePayByTolerance = 120
