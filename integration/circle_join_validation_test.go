//go:build integration

// circle_join_validation_test.go is how each of the Hub's join limits reaches a
// member through cashctl: which exit code and wallet code a refused join gets,
// and that a refusal leaves no pending join behind.
package integration

import (
	"testing"
	"time"
)

// TestCircleJoin_RefusedTerms asks for terms the Hub does not grant. Limits
// get_info reports (max_exp_secs, per_wallet_max_millis) are refused by
// cashctl before anything is sent (reason over_hub_limit, no nwc_code); the
// others only the Hub can judge.
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
			exit: 3, code: "invalid_input", nwcCode: "",
		},
		{
			// Under lokihub's minimum cap (its 11-loki fee reserve): the Hub's
			// LIMIT_EXCEEDED, retryable with other values (NIP-CW §Errors).
			name: "cap under the hub's minimum",
			args: []string{"0.5"},
			exit: 3, code: "invalid_input", nwcCode: "LIMIT_EXCEEDED",
		},
		{
			name: "cap over the per-wallet maximum",
			opts: circleHubOpts{PerWalletMaxMloki: 50_000},
			args: []string{"60"},
			exit: 3, code: "invalid_input", nwcCode: "",
		},
		// A cap over what the Hub holds, or over the Hub's own budget, is no
		// refusal: NIP-CW §Processing Algorithm checks only the per-wallet
		// ceilings and, at step 9, the circle's holdings ceiling.
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
	joined := f.mustJSON("join", "20", *hub.CircleHubToken)
	resp, _ := joined["response"].(map[string]any)
	if got, _ := resp["max_amount_millis"].(float64); got != 20_000 {
		t.Errorf("max_amount_millis = %v, want 20000: %v", resp["max_amount_millis"], resp)
	}
}

// TestCircleJoin_ExpiredWalletDoesNotBlockRejoin: one wallet per member and
// Hub counts only an Active or Suspended wallet (NIP-CW §Processing Algorithm,
// step 5), so once the member's wallet has expired a new join succeeds, while
// a second join next to a live wallet is still exists.
func TestCircleJoin_ExpiredWalletDoesNotBlockRejoin(t *testing.T) {
	admin := adminOrSkip(t)
	f, pub := newMember(t)
	hub := setUpCircleHub(t, admin, pub)

	f.mustJSON("join", *hub.CircleHubToken, "20", "--expiry", "3s")
	time.Sleep(5 * time.Second)

	f.mustJSON("join", *hub.CircleHubToken, "20")
	requireFailure(t, "a second join next to the live wallet", f.run("join", *hub.CircleHubToken, "20"), 9, "exists", "WALLET_EXISTS")
}
