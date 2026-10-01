package cmd

import (
	"encoding/json"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcash/transport"

	"github.com/ohstr/cashctl/internal/ledger"
)

// The pending-secret fallback, in the shape batching needs.
//
// spendCashEntry covers the single-call methods. Redeem stopped being a single
// call when it became a batch and the fallback was left behind — a regression
// nothing caught at unit level, because there was no unit-level test of the
// batch path at all. These are that test, at the level of the defect: the
// decision about which items qualify, made without a relay.

func secretPlan(id, cashSecret, pendingSecret string) redeemPlan {
	return redeemPlan{Entry: &ledger.Entry{
		ID:                id,
		CashSecret:        cashSecret,
		PendingCashSecret: pendingSecret,
	}}
}

func batchItem(id string) nipcashclient.BatchRedeem {
	// A Bill's halves cannot be set independently, so a fixture builds one the same
	// way production does — from a token's two fields.
	bill, err := nipcashclient.BillFromParts("wallet-"+id, "secret-"+id)
	if err != nil {
		panic(err)
	}
	return nipcashclient.BatchRedeem{ID: id, Bill: bill}
}

func declinedItem(id, code string) nipcashclient.RedeemOutcome {
	return nipcashclient.RedeemOutcome{ItemOutcome: nipcashclient.ItemOutcome{
		ID:    id,
		State: nipcashclient.OutcomeError,
		Error: &transport.ResultError{Code: code, Message: code},
	}}
}

func paidItem(id string) nipcashclient.RedeemOutcome {
	raw, _ := json.Marshal(nipcash.CashRedeemResult{Preimage: "beef"})
	return nipcashclient.RedeemOutcome{ItemOutcome: nipcashclient.ItemOutcome{
		ID:     id,
		State:  nipcashclient.OutcomeResult,
		Result: raw,
	}}
}

// A wrong-secret decline on an entry holding a live pending secret is retried,
// and a successful retry both replaces the outcome and promotes the secret.
//
// Promotion is the part worth pinning: without it the entry keeps the dead
// secret and the next spend fails again, which makes the recovery good for
// exactly one call.
func TestRetryWithPendingSecrets_RecoversAndPromotes(t *testing.T) {
	plans := []redeemPlan{secretPlan("tok-a", "dead", "live")}
	items := []nipcashclient.BatchRedeem{batchItem("tok-a")}
	results := []nipcashclient.RedeemOutcome{declinedItem("tok-a", "NOT_FOUND")}

	var sentCreds []nipcash.Credential
	out := retryWithPendingSecrets(plans, items, results, func(r []nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
		for _, it := range r {
			sentCreds = append(sentCreds, it.Params.Credential)
		}
		return []nipcashclient.RedeemOutcome{paidItem("tok-a")}
	})

	if len(sentCreds) != 1 {
		t.Fatalf("retried %d item(s), want exactly 1", len(sentCreds))
	}
	if len(out) != 1 || !out[0].Succeeded() {
		t.Fatalf("a successful retry must replace the decline: %+v", out)
	}
	if got := plans[0].Entry.CashSecret; got != "live" {
		t.Errorf("cash_secret = %q, want the promoted %q — otherwise the next spend fails again", got, "live")
	}
	if got := plans[0].Entry.PendingCashSecret; got != "" {
		t.Errorf("pending_cash_secret = %q, want it cleared once reconciled", got)
	}
}

// A failed retry must leave the ORIGINAL decline in place and must not promote.
// A second guess's own failure adds nothing, and promoting on it would write a
// secret now known not to work.
func TestRetryWithPendingSecrets_FailedRetryKeepsTheFirstDecline(t *testing.T) {
	plans := []redeemPlan{secretPlan("tok-a", "dead", "also-dead")}
	items := []nipcashclient.BatchRedeem{batchItem("tok-a")}
	results := []nipcashclient.RedeemOutcome{declinedItem("tok-a", "NOT_FOUND")}

	out := retryWithPendingSecrets(plans, items, results, func([]nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
		return []nipcashclient.RedeemOutcome{declinedItem("tok-a", "NOT_FOUND")}
	})

	if len(out) != 1 || out[0].Succeeded() {
		t.Fatalf("a failed retry must not turn into a success: %+v", out)
	}
	if plans[0].Entry.CashSecret != "dead" || plans[0].Entry.PendingCashSecret != "also-dead" {
		t.Errorf("a failed retry must not promote: cash_secret=%q pending=%q",
			plans[0].Entry.CashSecret, plans[0].Entry.PendingCashSecret)
	}
}

// An OMISSION is never retried, whatever secrets the entry holds. This is the
// safety property, not an optimisation: an omission is indistinguishable from a
// redemption whose reply was lost, so resending it could pay twice.
func TestRetryWithPendingSecrets_NeverRetriesAnOmission(t *testing.T) {
	plans := []redeemPlan{secretPlan("tok-a", "dead", "live")}
	items := []nipcashclient.BatchRedeem{batchItem("tok-a")}
	results := []nipcashclient.RedeemOutcome{{ItemOutcome: nipcashclient.ItemOutcome{
		ID: "tok-a", State: nipcashclient.OutcomeNotServed,
	}}}

	called := false
	out := retryWithPendingSecrets(plans, items, results, func([]nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
		called = true
		return nil
	})

	if called {
		t.Fatal("an omission must never be resent — it may already have been paid")
	}
	if len(out) != 1 || out[0].State != nipcashclient.OutcomeNotServed {
		t.Fatalf("the omission must survive unchanged: %+v", out)
	}
}

// Errors OTHER than a wrong-secret decline are not retried. Retrying an expired
// or over-budget bill with a different secret cannot help and only spends
// another slice of the shared rate limit.
func TestRetryWithPendingSecrets_OnlyWrongSecretDeclines(t *testing.T) {
	plans := []redeemPlan{secretPlan("tok-a", "dead", "live")}
	items := []nipcashclient.BatchRedeem{batchItem("tok-a")}

	for _, code := range []string{"EXPIRED", "QUOTA_EXCEEDED", "RATE_LIMITED", "INTERNAL"} {
		called := false
		retryWithPendingSecrets(plans, items,
			[]nipcashclient.RedeemOutcome{declinedItem("tok-a", code)},
			func([]nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
				called = true
				return nil
			})
		if called {
			t.Errorf("%s must not trigger a pending-secret retry", code)
		}
	}
}

// No pending secret, or one identical to the live one, means there is nothing to
// reconcile and no second guess to make.
func TestRetryWithPendingSecrets_NothingToReconcile(t *testing.T) {
	cases := []struct {
		name          string
		cash, pending string
	}{
		{"no pending secret", "dead", ""},
		{"pending equals live", "dead", "dead"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plans := []redeemPlan{secretPlan("tok-a", tc.cash, tc.pending)}
			called := false
			retryWithPendingSecrets(plans,
				[]nipcashclient.BatchRedeem{batchItem("tok-a")},
				[]nipcashclient.RedeemOutcome{declinedItem("tok-a", "NOT_FOUND")},
				func([]nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
					called = true
					return nil
				})
			if called {
				t.Error("nothing to reconcile, so nothing should be resent")
			}
		})
	}
}

// Only the qualifying item of a mixed batch is retried. The point of doing this
// as a batch is that one bill's recovery must not resend the others — several of
// which succeeded, and resending a success is a double-spend.
func TestRetryWithPendingSecrets_RetriesOnlyTheQualifyingItem(t *testing.T) {
	plans := []redeemPlan{
		secretPlan("tok-ok", "fine", ""),
		secretPlan("tok-recover", "dead", "live"),
		secretPlan("tok-omitted", "dead", "live"),
	}
	items := []nipcashclient.BatchRedeem{batchItem("tok-ok"), batchItem("tok-recover"), batchItem("tok-omitted")}
	results := []nipcashclient.RedeemOutcome{
		paidItem("tok-ok"),
		declinedItem("tok-recover", "NOT_FOUND"),
		{ItemOutcome: nipcashclient.ItemOutcome{ID: "tok-omitted", State: nipcashclient.OutcomeNotServed}},
	}

	var sentIDs []string
	out := retryWithPendingSecrets(plans, items, results, func(r []nipcashclient.BatchRedeem) []nipcashclient.RedeemOutcome {
		for _, it := range r {
			sentIDs = append(sentIDs, it.ID)
		}
		return []nipcashclient.RedeemOutcome{paidItem("tok-recover")}
	})

	if len(sentIDs) != 1 || sentIDs[0] != "tok-recover" {
		t.Fatalf("retried %v, want only [tok-recover]", sentIDs)
	}
	byID := map[string]nipcashclient.RedeemOutcome{}
	for _, o := range out {
		byID[o.ID] = o
	}
	if !byID["tok-ok"].Succeeded() {
		t.Error("an already-successful item must be left alone")
	}
	if !byID["tok-recover"].Succeeded() {
		t.Error("the recovered item must now read as successful")
	}
	if byID["tok-omitted"].State != nipcashclient.OutcomeNotServed {
		t.Error("the omitted item must stay omitted")
	}
}
