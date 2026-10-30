//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"
)

// TestCircleJoin_RequestReusedAfterLostReplyRecoversWallet is the live
// evidence for NIP-CW §Lost Join Reply / §Recovering a Lost Connection: a
// circle join whose reply is lost AFTER the Hub already made the wallet is
// recovered by `cash settle`'s lookup — the wallet is installed and usable,
// and no second, stranded wallet is left behind.
//
// Setup: an ALLOWLISTED circle_hub (setUpCircleHub authorizes this member's
// own key), so the join is ACCEPTED and a wallet is really created. The join
// is sent and then cashctl is killed right after the send
// (CASHCTL_CRASHPOINT=calls.after_send — which fires on the
// create_circle_wallet publish via cmd/session.go callContext's after-publish
// hook, since joinJournal.WriteAhead marks the call written ahead before the
// send). So the Hub creates the wallet but cashctl never reads the reply:
// exactly the lost-reply shape e2e_member_requests_test.go drives for
// cash_order / cash_withdraw, recovered here for a circle join.
//
// On the exact REQUEST_REUSED wire (the ledger item's name): the Hub returns
// REQUEST_REUSED only to a RESEND of create_circle_wallet carrying the same
// identity_event (nipcw.ErrRequestReused -> nipcw/client.Join's
// *JoinIndeterminateError -> recover via LookupJoin). That resend is an
// in-process, transport-level retry; the black-box CLI cannot reproduce it,
// because every `cashctl join` mints a FRESH connection key and signs a fresh
// identity_event (so it can never reuse a request id), and the crashpoint
// exits the process before any in-process resend. That precise
// resend -> REQUEST_REUSED -> lookup-done path is covered as a unit test in
// nmilat: nipcw/client/join_test.go (the REQUEST_REUSED case -> indeterminate)
// and nipcw/client/ejoin_test.go ("own is REQUEST_REUSED, and the lookup is
// done"). What IS black-box reachable — and what this test pins — is the same
// recovery's observable outcome: settle's lookup finds the already-made
// wallet, installs it once, and strands nothing.
func TestCircleJoin_RequestReusedAfterLostReplyRecoversWallet(t *testing.T) {
	admin := adminOrSkip(t)
	f, pubHex := newMember(t)
	hub := setUpCircleHub(t, admin, pubHex)
	if hub.CircleHubToken == nil || *hub.CircleHubToken == "" {
		t.Fatalf("ephemeral circle_hub has no circlehub1 connection: %+v", hub)
	}

	// Join, killed right after create_circle_wallet was sent: the Hub makes
	// the wallet, cashctl never sees the reply.
	res := f.runWith([]string{"CASHCTL_CRASHPOINT=calls.after_send"}, "",
		"join", "--hub", *hub.CircleHubToken, "--max-amount", "100", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("the crashpoint did not fire (join returned cleanly): %s", res.Stdout)
	}

	// The join is written ahead but unfinished: one open pending join, and no
	// wallet registered yet (a fresh member fixture starts with none).
	show := f.mustJSON("wallet", "show")
	if pj, _ := show["pending_joins"].([]any); len(pj) != 1 {
		t.Fatalf("after a lost join reply: pending_joins = %v, want exactly one open join", pj)
	}
	if ws, _ := show["wallets"].([]any); len(ws) != 0 {
		t.Fatalf("after a lost join reply: wallets = %v, want none registered yet", ws)
	}

	// `cash settle` recovers it, but only once the join's deadline has passed:
	// settleJoins (cmd/circle_lookup.go) reports a JoinWritten join as "not due
	// yet" until now >= j.Deadline+1, and j.Deadline is createdAt + JoinFreshness
	// (nmilat nipcw.JoinFreshness = 300s) — a deliberate client-side wait, since
	// the original join reply may still arrive within the proof's freshness
	// window. There is no way to shorten it from the black-box CLI: the gate is
	// cashctl's own time.Now(), the deadline is baked into the pending-join
	// record at write-ahead, and `cash settle --wait` waits only on ledger
	// Calls, never on joins. So the loop must wall-clock past ~300s; the 7m
	// budget clears it with margin on a loaded stack. Once due, the lookup
	// (cash_lookup over the private transport) finds the wallet the Hub already
	// made and installs it. "not_applied" would mean the Hub never made it —
	// for a join that WAS sent, a real failure.
	var walletName string
	eventually(t, 7*time.Minute, "lost join recovered by cash settle (after the 300s join-freshness window)", func() bool {
		var out struct {
			Joins []struct {
				State  string `json:"state"`
				Wallet string `json:"wallet"`
			} `json:"joins"`
		}
		_ = json.Unmarshal([]byte(f.run("cash", "settle", "--no-wait", "--yes").Stdout), &out)
		for _, j := range out.Joins {
			if j.State == "not_applied" {
				t.Fatalf("settle reports the Hub never made the wallet, but the join was sent: %+v", j)
			}
			if j.State == "done" && j.Wallet != "" {
				walletName = j.Wallet
				return true
			}
		}
		return false
	})

	// No stranded/duplicate wallet: exactly one wallet, and no join left open.
	show = f.mustJSON("wallet", "show")
	if pj, _ := show["pending_joins"].([]any); len(pj) != 0 {
		t.Errorf("after recovery: pending_joins = %v, want none (the join is finished)", pj)
	}
	if ws, _ := show["wallets"].([]any); len(ws) != 1 {
		t.Fatalf("after recovery: wallets = %v, want exactly one (no stranded duplicate)", ws)
	}

	// The recovered wallet is real and usable: it answers get-info.
	info := requireOK(t, "wallet get-info on the recovered wallet", f.run("-c", walletName, "wallet", "get-info"))
	if info["alias"] == nil && info["methods"] == nil {
		t.Errorf("recovered wallet get-info doesn't look like a real get_info result: %v", info)
	}
}
