//go:build integration

// circle_join_validation_test.go is how each of the Hub's join limits reaches a
// member through cashctl: which exit code and wallet code a refused join gets,
// and that a refusal leaves no pending join behind.
package integration

import (
	"testing"
	"time"
)

// TestCircleJoin_RefusedTerms asks for terms the Hub does not grant.
func TestCircleJoin_RefusedTerms(t *testing.T) {
	admin := adminOrSkip(t)

	for _, tc := range []struct {
		name    string
		opts    circleHubOpts
		args    []string
		exit    int
		code    string
		nwcCode string
	}{
		{
			name: "expiry over the hub's maximum",
			opts: circleHubOpts{MaxExpSecs: 3600},
			args: []string{"10", "--expiry", "2h"},
			exit: 3, code: "invalid_input", nwcCode: "BAD_REQUEST",
		},
		{
			name: "cap under one loki",
			args: []string{"0.5"},
			exit: 3, code: "invalid_input", nwcCode: "BAD_REQUEST",
		},
		{
			name: "cap over the per-wallet maximum",
			opts: circleHubOpts{PerWalletMaxMloki: 50_000},
			args: []string{"60"},
			exit: 1, code: "internal", nwcCode: "QUOTA_EXCEEDED",
		},
		{
			name: "cap over what the hub holds",
			opts: circleHubOpts{FundLoki: 100},
			args: []string{"200"},
			exit: 1, code: "internal", nwcCode: "QUOTA_EXCEEDED",
		},
		{
			name: "cap over the hub's own budget",
			opts: circleHubOpts{FundLoki: 100, HubMaxAmountLoki: 50},
			args: []string{"80"},
			exit: 1, code: "internal", nwcCode: "QUOTA_EXCEEDED",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, pub := newMember(t)
			hub := setUpCircleHubOpts(t, admin, pub, tc.opts)
			res := f.run(append([]string{"join", *hub.CircleHubToken}, tc.args...)...)
			requireFailure(t, "join", res, tc.exit, tc.code, tc.nwcCode)
			requirePendingJoinsEmpty(t, "refused join", f)
			if list, _ := f.mustJSON("connect", "list")["connections"].([]any); len(list) != 0 {
				t.Errorf("a refused join registered a wallet: %v", list)
			}
		})
	}

	t.Run("zero cap is refused before anything is sent", func(t *testing.T) {
		f, pub := newMember(t)
		hub := setUpCircleHub(t, admin, pub)
		requireFailure(t, "join 0", f.run("join", *hub.CircleHubToken, "0"), 3, "invalid_input", "")
		if children, err := admin.listCircleChildren(hub.ID); err != nil || len(children) != 0 {
			t.Errorf("hub children after a zero-cap join = %v (err %v), want none", children, err)
		}
	})
}

// TestCircleJoin_AmountFirstPositional joins with the amount before the hub
// connection: the two positionals are told apart by shape, not position.
func TestCircleJoin_AmountFirstPositional(t *testing.T) {
	admin := adminOrSkip(t)
	f, pub := newMember(t)
	hub := setUpCircleHub(t, admin, pub)
	joined := f.mustJSON("join", "10", *hub.CircleHubToken)
	resp, _ := joined["response"].(map[string]any)
	if got, _ := resp["max_amount_millis"].(float64); got != 10_000 {
		t.Errorf("max_amount_millis = %v, want 10000: %v", resp["max_amount_millis"], resp)
	}
}

// TestCircleJoin_ExpiredWalletBlocksRejoinUntilDeleted: an expired circle
// wallet still counts as the member's one wallet, so a re-join is `exists`
// until the host deletes it, after which the member can join again.
func TestCircleJoin_ExpiredWalletBlocksRejoinUntilDeleted(t *testing.T) {
	admin := adminOrSkip(t)
	f, pub := newMember(t)
	hub := setUpCircleHub(t, admin, pub)

	f.mustJSON("join", *hub.CircleHubToken, "10", "--expiry", "3s")
	time.Sleep(5 * time.Second)

	requireFailure(t, "re-join while the expired wallet exists", f.run("join", *hub.CircleHubToken, "10"), 9, "exists", "WALLET_EXISTS")

	children, err := admin.listCircleChildren(hub.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("hub children = %v (err %v), want the one expired wallet", children, err)
	}
	if err := admin.deleteCircleChild(hub.ID, children[0].AppID); err != nil {
		t.Fatalf("delete the expired wallet: %v", err)
	}
	f.mustJSON("join", *hub.CircleHubToken, "10")
}
