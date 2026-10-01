//go:build integration

// NIP-CASH conformance: the method and scope surface.
//
// Each test names the normative sentence it checks and asserts it BEHAVIOURALLY — a
// requirement counts as met only if the test would fail when it is violated. Reading the
// implementation and agreeing with it is not evidence: the standard transport served bill
// methods for months while the spec called the private one OPTIONAL, and neither
// statement looked wrong on its own.
//
// Wire-level requirements — what the Hub refuses on kind 23194, what get_info advertises,
// what reaches the relay — live in lokihub's own integration suite, which can make raw
// protocol calls. What is here is what a CLIENT can observe.
package integration

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcash"
)

// §Scoping the Roster: "Absent means `mine`" and "A Hub MUST NOT widen to the full
// roster on no match".
//
// Both halves on one bill, so the pair cannot drift: the same caller asking two ways
// must get two different answers, and a caller holding no slice must get neither.
func TestSpec_ScopingTheRoster_DefaultIsMineAndNoMatchDoesNotWiden(t *testing.T) {
	_, hub := plainClientHub(t)
	privA, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	const amountA, amountB = uint64(40_000), uint64(60_000)
	token := mintTwoRecipientBill(t, hub, pubA, pubB, amountA, amountB)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c := plainClient(t, ctx, token)

	// Absent scope.
	bare, err := c.CashStatus(ctx, nipcash.BySigning(privA), "")
	if err != nil {
		t.Fatalf("cash_status with no scope: %v", err)
	}
	if len(bare.Recipients) != 1 || bare.Recipients[0].IdentityValue != pubA {
		t.Fatalf("absent scope must mean `mine`, got %+v", bare.Recipients)
	}

	// Explicit all — the wider view is still available on request.
	all, err := c.CashStatus(ctx, nipcash.BySigning(privA), nipcash.ScopeAll)
	if err != nil {
		t.Fatalf("cash_status scope=all: %v", err)
	}
	if len(all.Recipients) != 2 {
		t.Errorf("scope=all must return the shared roster, got %d rows", len(all.Recipients))
	}

	// No match must not widen. A verified proof from someone holding no slice.
	strangerPriv, _ := newIdentity(t)
	res, err := c.CashStatus(ctx, nipcash.BySigning(strangerPriv), nipcash.ScopeAll)
	if err == nil && res != nil && len(res.Recipients) > 0 {
		t.Fatalf("a caller holding no slice received %d roster row(s) — the Hub widened on no match: %+v",
			len(res.Recipients), res.Recipients)
	}
	if err != nil && strings.Contains(err.Error(), pubB) {
		t.Errorf("the refusal leaked a co-recipient: %v", err)
	}
}

// §Scoping the Roster: "A Hub MUST reject a value that is neither `all` nor `mine`."
//
// Rejected, not silently coerced. A hub that fell back to a default would answer a
// question the caller did not ask — and if it fell back to `all`, it would disclose
// strictly more than was requested.
func TestSpec_ScopingTheRoster_UnknownScopeIsRejected(t *testing.T) {
	_, hub := plainClientHub(t)
	privA, pubA := newIdentity(t)
	_, pubB := newIdentity(t)
	token := mintTwoRecipientBill(t, hub, pubA, pubB, 40_000, 60_000)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	_, err := plainClient(t, ctx, token).CashStatus(ctx, nipcash.BySigning(privA), "everything")
	if err == nil {
		t.Fatal("an unknown scope was accepted; the spec requires it be rejected")
	}
	t.Logf("unknown scope rejected: %v", err)
}
