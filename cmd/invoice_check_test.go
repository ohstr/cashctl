package cmd

import (
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
)

func TestValidateInvoiceShape(t *testing.T) {
	// Real BOLT11 invoices are bech32 with a long data part; the checksum is
	// all this check verifies, so any well-formed bech32 under an ln* HRP does.
	valid := func(hrp string) string {
		s, err := bech32.Encode(hrp, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
		if err != nil {
			t.Fatalf("bech32.Encode(%q): %v", hrp, err)
		}
		return s
	}
	for _, hrp := range []string{"lnbc", "lntb", "lnfc", "lnbcrt"} {
		if err := validateInvoiceShape(valid(hrp)); err != nil {
			t.Errorf("validateInvoiceShape(%s invoice) = %v, want accepted", hrp, err)
		}
	}
	if err := validateInvoiceShape("  " + valid("lnbc") + "\n"); err != nil {
		t.Errorf("surrounding whitespace should be tolerated: %v", err)
	}

	for _, bad := range []string{
		"", "garbage", "lnfc1x", "lnbc", // not bech32 / too short
		valid("lnbc")[:len(valid("lnbc"))-1] + "q", // right shape, wrong checksum
		valid("npub"),     // valid bech32 but not an invoice
		valid("lokicash"), // a cash token pasted by mistake
	} {
		if err := validateInvoiceShape(bad); err == nil {
			t.Errorf("validateInvoiceShape(%.30q) = nil, want rejected", bad)
		}
	}
}

// TestBolt11AmountMloki covers the BOLT11 amount-in-the-HRP decode `pay`'s
// new confirmation preview relies on — every multiplier the spec defines,
// the chain-prefix-length independence (lnbcrt's "bcrt" vs lnbc's "bc"),
// the legal amountless case, and the overflow guard mirroring
// ParseAmount's own (internal/output/output.go).
func TestBolt11AmountMloki(t *testing.T) {
	encode := func(hrp string) string {
		s, err := bech32.Encode(hrp, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12})
		if err != nil {
			t.Fatalf("bech32.Encode(%q): %v", hrp, err)
		}
		return s
	}

	cases := []struct {
		hrp      string
		wantNil  bool
		wantMsat int64
	}{
		{hrp: "lnbc543210n", wantMsat: 54_321_000},  // nano
		{hrp: "lnbc2500u", wantMsat: 250_000_000},   // micro
		{hrp: "lnbc1m", wantMsat: 100_000_000},      // milli
		{hrp: "lnbc7", wantMsat: 700_000_000_000},   // whole coin, no multiplier
		{hrp: "lnbcrt2500u", wantMsat: 250_000_000}, // multi-letter chain prefix ("bcrt")
		{hrp: "lnbc10p", wantMsat: 1},               // pico, exact whole msat
		{hrp: "lnbc", wantNil: true},                // amountless: legal, no preview
		{hrp: "lnbcrt", wantNil: true},              // amountless, multi-letter prefix
		{hrp: "lnbcm", wantNil: true},               // no digit anywhere — amountless, not a malformed "m" amount
	}
	for _, c := range cases {
		got, err := bolt11AmountMloki(encode(c.hrp))
		if err != nil {
			t.Errorf("bolt11AmountMloki(%s) error = %v, want nil", c.hrp, err)
			continue
		}
		if c.wantNil {
			if got != nil {
				t.Errorf("bolt11AmountMloki(%s) = %d, want nil (amountless)", c.hrp, *got)
			}
			continue
		}
		if got == nil || *got != c.wantMsat {
			t.Errorf("bolt11AmountMloki(%s) = %v, want %d", c.hrp, got, c.wantMsat)
		}
	}

	errCases := []string{
		"lnbc15p",          // pico that isn't a whole msat (15/10 isn't integral)
		"lnbc5x",           // 'x' isn't a BOLT11 multiplier
		"lnbc100000000000", // whole-coin scale: parses fine alone, overflows once multiplied
	}
	for _, hrp := range errCases {
		got, err := bolt11AmountMloki(encode(hrp))
		if err == nil {
			t.Errorf("bolt11AmountMloki(%s) = %v, <nil>, want an error", hrp, got)
		}
	}

	if _, err := bolt11AmountMloki("not-a-bech32-invoice"); err == nil {
		t.Error("bolt11AmountMloki(garbage) = nil error, want rejected")
	}
}
