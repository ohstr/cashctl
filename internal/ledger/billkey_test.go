package ledger

import (
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/testmint"
)

func TestAdd_ABillRespelledIsNotASecondHolding(t *testing.T) {
	bill := testmint.Bill(t, nipcash.Token{}, "")
	tok, err := nipcash.Decode(bill)
	if err != nil {
		t.Fatal(err)
	}
	for name, other := range map[string]string{
		"an unknown TLV appended": testmint.WithUnknownTLV(t, bill),
		"entries reordered":       testmint.Reordered(t, bill),
		"uppercase":               strings.ToUpper(bill),
		"uppercase, reordered":    strings.ToUpper(testmint.Reordered(t, bill)),
	} {
		t.Run(name, func(t *testing.T) {
			if other == bill {
				t.Fatal("setup: the spelling is the same string")
			}
			if _, ok := nipcash.VerifyProvenance(mustDecode(t, other)); !ok {
				t.Fatal("setup: the other spelling must still verify")
			}
			amt := uint64(1000)
			l := &Ledger{}
			if _, err := l.Add(Entry{Token: bill, WalletPubkey: tok.WalletPubkey, AmountMillis: &amt}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Add(Entry{Token: other, WalletPubkey: tok.WalletPubkey, AmountMillis: &amt}); !errors.Is(err, ErrAlreadyHeld) {
				t.Fatalf("the same bill re-spelled: err = %v, held = %d; want ErrAlreadyHeld", err, len(l.Held()))
			}
			if e, ok := l.FindByToken(other); !ok || e.Token != bill {
				t.Errorf("FindByToken(other spelling) = %v, %v; want the held row", e, ok)
			}
		})
	}
}

func TestAdd_StoresTheOneSpelling(t *testing.T) {
	bill := testmint.Bill(t, nipcash.Token{}, "")
	l := &Ledger{}
	e, err := l.Add(Entry{Token: testmint.WithUnknownTLV(t, testmint.Reordered(t, bill)), WalletPubkey: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Token != bill {
		t.Errorf("stored token = %s, want the canonical %s", e.Token, bill)
	}
	if e.WalletPubkey != mustDecode(t, bill).WalletPubkey {
		t.Errorf("wallet pubkey = %q, want the token's own", e.WalletPubkey)
	}
}

func TestAdd_AnotherBillAndALetGoBillStillGoIn(t *testing.T) {
	bill := testmint.Bill(t, nipcash.Token{}, "")
	l := &Ledger{}
	first, err := l.Add(Entry{Token: bill})
	if err != nil {
		t.Fatal(err)
	}
	// Another bill, even on the same Hub and relays, is a second holding.
	if _, err := l.Add(Entry{Token: testmint.Bill(t, nipcash.Token{}, "")}); err != nil {
		t.Fatalf("a different bill was refused: %v", err)
	}
	// A bill let go and come back, spelled another way, reactivates its row.
	firstID := first.ID
	if err := l.SetStatus(firstID, StatusTransferred); err != nil {
		t.Fatal(err)
	}
	back, err := l.Add(Entry{Token: testmint.WithUnknownTLV(t, bill)})
	if err != nil {
		t.Fatalf("a bill let go came back and was refused: %v", err)
	}
	if back.ID != firstID || back.Status != StatusHeld || back.Token != bill || len(l.Entries) != 2 {
		t.Errorf("came back as %s %s (%d rows); want row %s held again in its one spelling", back.ID, back.Status, len(l.Entries), firstID)
	}
	// A live bill in any live state blocks a second copy.
	for _, s := range []string{StatusRedeeming, StatusZapClaiming} {
		_ = l.SetStatus(firstID, s)
		if _, err := l.Add(Entry{Token: testmint.Reordered(t, bill)}); !errors.Is(err, ErrAlreadyHeld) {
			t.Errorf("%s: err = %v, want ErrAlreadyHeld", s, err)
		}
	}
}

func TestSave_TwoProcessesReceivingTwoSpellingsHoldOneBill(t *testing.T) {
	withTempConfigDir(t)
	bill := testmint.Bill(t, nipcash.Token{}, "")
	a, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	b, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Add(Entry{Token: bill}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Add(Entry{Token: testmint.WithUnknownTLV(t, bill)}); err != nil {
		t.Fatal(err)
	}
	if err := a.Save(); err != nil {
		t.Fatal(err)
	}
	if err := b.Save(); !errors.Is(err, ErrAlreadyHeld) {
		t.Fatalf("the second process's Save = %v, want ErrAlreadyHeld", err)
	}
	// Two different bills saved by two processes both stay.
	c, _ := Load()
	if _, err := c.Add(Entry{Token: testmint.Bill(t, nipcash.Token{}, "")}); err != nil {
		t.Fatal(err)
	}
	if err := c.Save(); err != nil {
		t.Fatalf("a different bill: %v", err)
	}
	final, _ := Load()
	if n := len(final.Held()); n != 2 {
		t.Errorf("held = %d, want 2", n)
	}
}

func TestSave_AMovedBillIsStoredInItsOneSpelling(t *testing.T) {
	withTempConfigDir(t)
	l, _ := Load()
	e, err := l.Add(Entry{Token: testmint.Bill(t, nipcash.Token{}, "")})
	if err != nil {
		t.Fatal(err)
	}
	moved := testmint.Bill(t, nipcash.Token{}, "")
	e.Token = strings.ToUpper(testmint.Reordered(t, moved))
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	got, _ := Load()
	if got.Entries[0].Token != moved || got.Entries[0].WalletPubkey != mustDecode(t, moved).WalletPubkey {
		t.Errorf("stored %s / %s, want the moved bill's one spelling and wallet", got.Entries[0].Token, got.Entries[0].WalletPubkey)
	}
}

func TestZapEventFor_FindsTheBillUnderAnySpelling(t *testing.T) {
	withTempConfigDir(t)
	bill := testmint.Bill(t, nipcash.Token{}, "")
	l, _ := Load()
	if _, err := l.Add(Entry{Token: bill, ZapEvent: `{"kind":9522}`}); err != nil {
		t.Fatal(err)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{bill, testmint.WithUnknownTLV(t, bill), strings.ToUpper(bill)} {
		if ev, err := ZapEventFor(s); err != nil || ev != `{"kind":9522}` {
			t.Errorf("ZapEventFor = %q, %v", ev, err)
		}
	}
	if ev, err := ZapEventFor(testmint.Bill(t, nipcash.Token{}, "")); err != nil || ev != "" {
		t.Errorf("another bill: ZapEventFor = %q, %v; want none", ev, err)
	}
}

func mustDecode(t *testing.T, s string) nipcash.Token {
	t.Helper()
	tok, err := nipcash.Decode(s)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
