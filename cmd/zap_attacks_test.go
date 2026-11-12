package cmd

// Adversarial ATTACK sweep on the Cash-Zap claim path (NIP-CZ, kind 9522),
// driven through `cash receive --zap-event`. Each test is a TRUE-NEGATIVE: it
// FAILS if the guard it names were removed. The claim path has two independent
// horns that bind a zap bill to its intended recipient, and every theft attempt
// below runs into one of them:
//
//   - the LOCAL recipient gate (receiveZap: `mine != z.Recipient.Value`), which
//     refuses, before any network, a 9522 whose `p` is not the local identity; and
//   - rule 9 (zapStatusMine → nipCZ.CheckStatus), the Hub's own row for the
//     claimant — the only proof the cash is theirs — which a lying `p` cannot forge
//     because the Hub bound the bill to the real recipient's pubkey.
//
// Keep p = victim → the local gate refuses (TestZapAttack_CopiedZapClaimedByWrongRecipient).
// Change p = attacker → rule 9 refuses (TestZapAttack_ReauthoredZapReusingForeignBill).
// Edit any tag of a signed 9522 → rule 1 (event signature) refuses
// (TestZapAttack_RetaggedBillBreaksEventSignature). Edit the signed zap context →
// rule 6/8 recover a DIFFERENT minter so trust never transfers
// (TestZapAttack_TamperedZapContextBreaksMinterBinding).
//
// Untrusted-minter gating (attack 3) is proven by TestReceiveZap_ApprovalThenRule9;
// rule-9 claimed/shared/gone/no-answer by TestReceiveZap_Rule9Failures; amount
// inflation by zap_amount_ceiling_test.go; bill≠event by
// TestZap_EventMustMatchBillAndTargetVerified.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// writeZap marshals ev to a temp file and returns the path, for --zap-event.
func writeZap(t *testing.T, ev *nip01.Event) string {
	t.Helper()
	b, _ := json.Marshal(ev)
	p := filepath.Join(t.TempDir(), "zap.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// zapToVictim is a valid kind-9522 to a FRESH victim pubkey (not the local
// identity), from f's minter — i.e. the exact 9522 an attacker would lift off a
// relay. Returns the event and the zap bill it carries.
func zapToVictim(t *testing.T, f *zapFixture) (*nip01.Event, string, string) {
	t.Helper()
	victimPriv, victimPub := testmint.Key(t)
	sender, _ := testmint.Key(t)
	vnote, err := nip01.NewSignedEvent(1, "victim note", victimPriv)
	if err != nil {
		t.Fatal(err)
	}
	ev, bill := testmint.CashZap(t, f.minterPriv, sender, vnote, 21000)
	return ev, bill, victimPub
}

// Attack 1 — COPIED / REPLAYED 9522: an attacker lifts a victim's valid kind-9522
// off a relay and tries to claim its cash as their own. The bill's `p` still
// names the victim, so the local recipient gate refuses before the Hub is ever
// dialled. DEFENDED by receiveZap's `mine != z.Recipient.Value`.
func TestZapAttack_CopiedZapClaimedByWrongRecipient(t *testing.T) {
	f := newZapFixture(t) // local identity = me (the attacker running receive)
	h := &fakeZapHub{res: f.mine(21000, false)}
	installZapHub(t, h)

	ev, bill, victimPub := zapToVictim(t, f)
	if victimPub == f.me {
		t.Fatal("victim must differ from the local identity")
	}

	// Claim it as the event, and as the bill with its event: both refused.
	raw, _ := json.Marshal(ev)
	for _, args := range [][]string{{string(raw)}, {bill}} {
		flags := map[string]string{"trust-minter": "true"}
		if len(args) == 1 && args[0] == bill {
			flags["zap-event"] = writeZap(t, ev)
		}
		_, err := runReceiveZap(t, args, flags)
		if err == nil || !strings.Contains(err.Error(), "not your identity") {
			t.Fatalf("FINDING: a victim's zap was claimable by a different recipient: err=%v", err)
		}
	}
	if h.dials != 0 {
		t.Errorf("FINDING: the Hub was contacted for a stolen zap (%d dials) — a copied 9522 should be refused offline", h.dials)
	}
	if held := ledgerView(testLoad(t)).Held(); len(held) != 0 {
		t.Errorf("FINDING: a copied zap was credited (%d held bills)", len(held))
	}
}

// Attack 2 — RE-TAGGED 9522 keeping the original signature: an attacker edits the
// `cash` tag to point at a richer bill (or edits the `amount`) on a signed 9522
// without re-signing. Any tag edit changes the event id, so rule 1 (the event
// signature) fails and nothing is contacted or credited. DEFENDED.
func TestZapAttack_RetaggedBillBreaksEventSignature(t *testing.T) {
	f := newZapFixture(t) // the valid 9522 here is to me, so we clear the recipient gate
	h := &fakeZapHub{res: f.mine(21000, false)}
	installZapHub(t, h)

	richAmt := uint64(9_000_000)
	richBill := testmint.Bill(t, nipcash.Token{HRP: "lokicash", IdentityRequired: ptrTo(true), AttestedAmountMillis: &richAmt}, f.minterPriv)

	for name, mutate := range map[string]func(ev *nip01.Event){
		"swap cash tag to a richer bill": func(ev *nip01.Event) { setTag(ev, "cash", richBill) },
		"inflate the amount tag":         func(ev *nip01.Event) { setTag(ev, "amount", "9000000") },
	} {
		t.Run(name, func(t *testing.T) {
			var ev nip01.Event
			_ = json.Unmarshal([]byte(f.raw), &ev)
			before := h.dials
			mutate(&ev) // keep the original signature — do NOT re-sign
			_, err := runReceiveZap(t, []string{writeZapRaw(t, &ev)}, map[string]string{"trust-minter": "true"})
			if err == nil || !strings.Contains(err.Error(), "rule 1") {
				t.Fatalf("FINDING: a re-tagged 9522 (orig signature) was accepted: err=%v", err)
			}
			if h.dials != before {
				t.Errorf("FINDING: a re-tagged 9522 reached the Hub (dials %d→%d)", before, h.dials)
			}
			if held := ledgerView(testLoad(t)).Held(); len(held) != 0 {
				t.Errorf("FINDING: a re-tagged 9522 was credited (%d held)", len(held))
			}
		})
	}
}

// Attack 2-deep / 4 — RE-AUTHORED 9522 reusing a victim's zap bill: the attacker
// re-wraps the victim's zap bill in a FRESH 9522 they sign themselves, with
// `p` = attacker, so it clears the local recipient gate. But the bill is bound at
// the Hub to the victim, so rule 9 (cash_status mine) finds no row for the
// attacker and refuses. DEFENDED by rule 9 — the ground truth the local `p`
// cannot forge. (Without --trust-minter it is already refused offline: the zap
// signature was never over the attacker's context, so rule 6 recovers a bogus,
// unlisted minter — the friction horn, asserted too.)
func TestZapAttack_ReauthoredZapReusingForeignBill(t *testing.T) {
	f := newZapFixture(t)
	vev, vbill, victimPub := zapToVictim(t, f)

	mePriv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	// A fresh 9522 the attacker signs, re-pointing the victim's bill at themselves.
	reEv, err := nip01.NewSignedEvent(9522, "steal", mePriv,
		[]string{"e", tagValue(vev, "e")}, []string{"k", "1"}, []string{"p", f.me},
		[]string{"amount", "21000"}, []string{"chain", "flokicoin"},
		[]string{"cash", vbill}, []string{"w", tagValue(vev, "w")},
		[]string{"issued_at", tagValue(vev, "issued_at")}, []string{"expires_at", "0"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(reEv)

	// Friction horn: without approval the re-authored zap's minter is unlisted
	// (the zap signature never covered the attacker's context), so it is refused
	// offline and the Hub is not contacted.
	hOff := &fakeZapHub{}
	installZapHub(t, hOff)
	_, err = runReceiveZap(t, []string{string(raw)}, nil)
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonMinterUntrusted {
		t.Fatalf("re-authored zap not stopped at the untrusted-minter gate: err=%v", err)
	}
	if hOff.dials != 0 {
		t.Errorf("FINDING: a re-authored zap reached the Hub before approval (%d dials)", hOff.dials)
	}

	// Ground-truth horn: even if the attacker trusts the (bogus) minter and the
	// Hub answers truthfully, the bill's only row is the VICTIM's, so rule 9
	// refuses. If this guard were removed the attacker would be credited.
	hOn := &fakeZapHub{res: &nipcash.CashStatusResult{Recipients: []nipcash.RecipientStatus{
		{IdentityType: "pubkey", IdentityValue: victimPub, AmountMillis: 21000, Claimed: false},
	}}}
	installZapHub(t, hOn)
	_, err = runReceiveZap(t, []string{string(raw)}, map[string]string{"trust-minter": "true"})
	if err == nil || !strings.Contains(err.Error(), "rule 9") {
		t.Fatalf("FINDING: a re-authored zap for a victim's bill was credited to the attacker: err=%v", err)
	}
	if held := ledgerView(testLoad(t)).Held(); len(held) != 0 {
		t.Errorf("FINDING: a re-authored zap was credited (%d held)", len(held))
	}
}

// Attack 4 — TAMPERED SIGNED CONTEXT (target / issued_at): the attacker re-authors
// their OWN-addressed 9522 (p stays = me, so the recipient gate and rule 9 Hub
// row would both pass) but changes a field the minter's zap signature covered —
// the target `e` or `issued_at`. Rule 6 then rebuilds a context the minter never
// signed and recovers a DIFFERENT key, so a trusted minter's trust does NOT carry
// over: rule 8 refuses. This proves the zap signature binds the target+times, not
// just the amount. DEFENDED.
func TestZapAttack_TamperedZapContextBreaksMinterBinding(t *testing.T) {
	f := newZapFixture(t)
	// Trust the REAL minter, so the ONLY thing that can stop the tampered copies
	// is the signature binding, not an untrusted-minter prompt.
	if err := config.TrustMinter(config.TrustedMinter{MinterXOnly: f.minterX, Chain: "flokicoin"}); err != nil {
		t.Fatal(err)
	}
	installZapHub(t, &fakeZapHub{res: f.mine(21000, false)})

	// Positive control: the genuine 9522 to me, real minter trusted, is received.
	if _, err := runReceiveZap(t, []string{f.raw}, nil); err != nil {
		t.Fatalf("control: a genuine trusted zap was refused: %v", err)
	}

	mePriv, _ := identity.Resolve(nil)
	issued, _ := strconv.ParseUint(tagValue(f.ev, "issued_at"), 10, 64)
	for name, tweak := range map[string]func() *nip01.Event{
		"swapped target e": func() *nip01.Event {
			ev, _ := nip01.NewSignedEvent(9522, "x", mePriv,
				[]string{"e", strings.Repeat("ab", 32)}, []string{"k", "1"}, []string{"p", f.me},
				[]string{"amount", "21000"}, []string{"chain", "flokicoin"},
				[]string{"cash", f.bill}, []string{"w", tagValue(f.ev, "w")},
				[]string{"issued_at", tagValue(f.ev, "issued_at")}, []string{"expires_at", "0"})
			return ev
		},
		"shifted issued_at": func() *nip01.Event {
			ev, _ := nip01.NewSignedEvent(9522, "x", mePriv,
				[]string{"e", tagValue(f.ev, "e")}, []string{"k", "1"}, []string{"p", f.me},
				[]string{"amount", "21000"}, []string{"chain", "flokicoin"},
				[]string{"cash", f.bill}, []string{"w", tagValue(f.ev, "w")},
				[]string{"issued_at", strconv.FormatUint(issued+99999, 10)}, []string{"expires_at", "0"})
			return ev
		},
	} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(tweak())
			// Real minter trusted, yet refused: the recovered minter differs, so
			// it reads as an unlisted minter (rule 8). trust-minter=false → offline.
			_, err := runReceiveZap(t, []string{string(raw)}, nil)
			if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonMinterUntrusted {
				t.Fatalf("FINDING: tampering the signed zap context kept the minter's trust: err=%v", err)
			}
		})
	}
}

// setTag replaces the value (element 1) of the first tag named name, in place.
func setTag(ev *nip01.Event, name, value string) {
	for i, tg := range ev.Tags {
		if len(tg) >= 2 && tg[0] == name {
			ev.Tags[i][1] = value
			return
		}
	}
}

func writeZapRaw(t *testing.T, ev *nip01.Event) string {
	t.Helper()
	b, _ := json.Marshal(ev)
	return string(b)
}
