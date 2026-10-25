package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// newTestReceiveCmd mirrors cash_transfer_test.go's own newTestTransferCmd
// — protectCashReceipt/checkClaimWithCashHub read these flags directly
// off cmd.
func newTestReceiveCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	return c
}

// TestProtectCashReceipt_NonCash_NotApplicable confirms the pure,
// no-network guard: a identity-bound entry never reaches the confirm prompt,
// the ledger, or a dial — it's the one branch of protectCashReceipt this
// package can unit-test directly (the rest are network-touching, covered
// by the integration suite instead, matching this codebase's existing
// split between unit and integration test responsibilities).
func TestProtectCashReceipt_NonCash_NotApplicable(t *testing.T) {
	c := newTestReceiveCmd()
	l := &ledger.Ledger{}
	entry := &ledger.Entry{ID: "tok-abcd"}

	status, finalEntry := protectCashReceipt(c, l, entry, false)

	if status.Status != "not_applicable" {
		t.Fatalf("Status = %q, want \"not_applicable\"", status.Status)
	}
	if finalEntry != entry {
		t.Fatal("finalEntry must be the same entry unchanged for a identity-bound receive")
	}
}

// TestProtectCashReceipt_Declined confirms declining the confirm prompt
// returns cleanly without ever reaching the ledger or a dial — entry has
// no real token, so a real attempt to build a RekeyCashSlice call from
// it would fail loudly (or hang), not return cleanly.
func TestProtectCashReceipt_Declined(t *testing.T) {
	c := newTestReceiveCmd()
	withStdin(t, "n\n")
	l := &ledger.Ledger{}
	entry := &ledger.Entry{ID: "tok-abcd", Token: "not-a-real-token"}

	status, finalEntry := protectCashReceipt(c, l, entry, true)

	if status.Status != "declined" {
		t.Fatalf("Status = %q, want \"declined\"", status.Status)
	}
	if finalEntry != entry {
		t.Fatal("finalEntry must be the same entry unchanged when declined")
	}
}

// TestPrintProtectFailure_PointsAtWalletProtect pins the remediation these
// two text-mode hints name. Both used to say `consolidate --to cash`, which
// cannot reach the holding either way: with no sources it auto-groups, and
// GroupableForConsolidation skips cash-mode entries entirely; with one it
// refuses ("needs at least 2 sources, got 1"). `wallet protect` exists for
// exactly this state. The string was last changed by a mechanical
// bearer->cash rename sweep, so a plain regression test is what keeps the
// next sweep from reintroducing advice that can't work.
func TestPrintProtectFailure_PointsAtWalletProtect(t *testing.T) {
	out := withCapturedStdout(func() { printProtectFailure(false, errors.New("hub declined")) })

	if !strings.Contains(out, "cashctl wallet protect") {
		t.Errorf("printProtectFailure must name `cashctl wallet protect`, got: %q", out)
	}
	if strings.Contains(out, "--to cash") {
		t.Errorf("printProtectFailure must not send the user to consolidate --to cash, got: %q", out)
	}
}

// TestPrintProtectPartialFailure_DoesNotSendToCashConsolidate covers the
// partial case, whose state is different: the interim step already
// reassigned the holding to the local pubkey (CashSecret cleared,
// IdentityRequired set), so nothing is still shared and only the merge
// failed. Naming a cash-mode target here was doubly wrong.
func TestPrintProtectPartialFailure_DoesNotSendToCashConsolidate(t *testing.T) {
	out := withCapturedStdout(func() { printProtectPartialFailure(false, errors.New("merge failed")) })

	if strings.Contains(out, "--to cash") {
		t.Errorf("printProtectPartialFailure must not name a cash-mode consolidate target, got: %q", out)
	}
}

// TestPrintProtectHints_SilentInJSONMode keeps these on the right side of
// AGENTS.md's output contract: they are text-mode narration, so --json
// callers must see nothing on stdout from them.
func TestPrintProtectHints_SilentInJSONMode(t *testing.T) {
	for name, fn := range map[string]func(){
		"printProtectFailure":        func() { printProtectFailure(true, errors.New("hub declined")) },
		"printProtectPartialFailure": func() { printProtectPartialFailure(true, errors.New("merge failed")) },
	} {
		if out := withCapturedStdout(fn); out != "" {
			t.Errorf("%s in jsonMode printed %q, want nothing", name, out)
		}
	}
}

// protectLedger is a fresh on-disk ledger holding one still-shared cash note,
// tok-a (secret sa, 1000), registered on h. Returns the ledger and its id.
func protectLedger(t *testing.T, h *fakeBillHub, tokens ...string) (*ledger.Ledger, []string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(tokens) == 0 {
		tokens = []string{"tok-a"}
	}
	var ids []string
	for _, tok := range tokens {
		secret := "s" + tok
		e, err := l.Add(ledger.Entry{Token: tok, WalletPubkey: "pk-" + tok, AmountMillis: ptrTo(uint64(1000)),
			IdentityRequired: ptrTo(false), CashSecret: secret, CashProtection: ledger.CashShared})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, e.ID)
		h.bill(tok, nipcash.BySecret(secret), 1000)
	}
	if err := l.Save(); err != nil {
		t.Fatal(err)
	}
	return l, ids
}

func reload(t *testing.T, id string) (*ledger.Ledger, *ledger.Entry) {
	t.Helper()
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	e, ok := l.Find(id)
	if !ok {
		t.Fatalf("%s missing after reload", id)
	}
	return l, e
}

// T-01 (B4): protect #1 commits on the Hub but its reply is lost. Protect #2
// must never write a new secret over the pending one: in place, it settles that
// the first landed and places no call; after a carve (the old token is gone and
// the new one was never learned), it refuses, so the pending secret, the only
// one the carved bill answers to, stays on disk.
func TestProtect_NeverOverwritesAnUnconfirmedReKey(t *testing.T) {
	for _, carve := range []bool{false, true} {
		t.Run(fmt.Sprintf("carve=%v", carve), func(t *testing.T) {
			h := newFakeBillHub(t)
			h.carve = carve
			l, ids := protectLedger(t, h)
			cmd := newTestReceiveCmd()
			_ = cmd.Flags().Set("json", "true")

			h.dropReplies = 1
			e, _ := l.Find(ids[0])
			if st, _ := protectRekeyOnly(cmd, l, e, true); !st.PendingSecretUnresolved {
				t.Fatalf("protect #1 with its reply lost: %+v; want pending_secret_unresolved", st)
			}
			l, e = reload(t, ids[0])
			s1 := e.PendingCashSecret
			if s1 == "" || e.CashSecret != "stok-a" {
				t.Fatalf("after protect #1: secret %q pending %q", e.CashSecret, s1)
			}

			st, _ := protectRekeyOnly(cmd, l, e, true)
			if n := h.moveCount(); n != 1 {
				t.Errorf("protect #2 placed a call (%d moves): it may overwrite the only working secret", n)
			}
			_, e = reload(t, ids[0])
			if carve {
				if st.Status != "failed" || !st.Unresolved {
					t.Errorf("protect #2 after a carve: %+v; want failed, unresolved", st)
				}
				if e.PendingCashSecret != s1 {
					t.Errorf("pending secret %q, want %q kept", e.PendingCashSecret, s1)
				}
				live := h.liveTokens()
				if len(live) != 1 || h.owner(live[0]) != nipcash.BySecret(s1) {
					t.Errorf("the carved bill %v is not spent by the kept pending secret", live)
				}
				return
			}
			if st.Status != "rekeyed" {
				t.Errorf("protect #2 in place: %+v; want rekeyed", st)
			}
			if e.CashSecret != s1 || e.PendingCashSecret != "" || e.CashProtection != ledger.CashProtected {
				t.Errorf("after protect #2: secret %q pending %q protection %q; want %q, none, protected",
					e.CashSecret, e.PendingCashSecret, e.CashProtection, s1)
			}
		})
	}
}

// T-03 (B1): a confirmed carve whose final save fails hands over the new
// <token>#<secret>: disk still names the old token, whose wallet is gone.
func TestProtect_UnsavedCarveReturnsTheNewTokenAsRecovery(t *testing.T) {
	h := newFakeBillHub(t)
	h.carve = true
	l, ids := protectLedger(t, h)
	h.afterCommit = func() { failSaves(t) }
	e, _ := l.Find(ids[0])

	st, _ := protectRekeyOnly(newTestReceiveCmd(), l, e, true)
	live := h.liveTokens()
	if len(live) != 1 {
		t.Fatalf("live bills %v, want the one carved bill", live)
	}
	tok, secret, ok := strings.Cut(st.Recovery, "#")
	if !ok || tok != live[0] || h.owner(tok) != nipcash.BySecret(secret) {
		t.Fatalf("recovery %q does not reach the carved bill %s", st.Recovery, live[0])
	}
	err := classifyProtectError(newTestReceiveCmd(), protectOutcome{ID: ids[0], Status: st})
	if got := output.AsCLIError(err).Recovery; got != st.Recovery {
		t.Errorf("error recovery %q, want %q", got, st.Recovery)
	}
}

// T-04 (B2): an unconfirmed re-key never says "no action needed".
func TestPrintProtectAmbiguousFailure_NeverSaysNoActionNeeded(t *testing.T) {
	out := withCapturedStdout(func() { printProtectAmbiguousFailure(false, context.DeadlineExceeded) })
	if strings.Contains(out, "no action needed") {
		t.Errorf("ambiguous re-key text says no action is needed: %q", out)
	}
	for _, want := range []string{"It may have worked.", "settles it before your next spend", "don't hand out the old code again"} {
		if !strings.Contains(out, want) {
			t.Errorf("ambiguous re-key text is missing %q: %q", want, out)
		}
	}
}
