//go:build integration

// e2e_member_requests_test.go drives, against lokihub's regtest dev stack, the
// NIP-CW member requests (rotate, freeze, unfreeze: signed by `cashctl circle
// request`, submitted through the host's admin API as the circle's host
// would), a member's own `wallet delete`, and the lost replies of `cash order
// place` and `cash withdraw send` made real by killing cashctl right after it
// sent the call (a crashpoints build).
//
// lokihub routes used: POST /api/apps (circle_hub with circleRotationEnabled,
// circleRotationDelaySecs), POST /api/circle-hubs/:id/member-requests {event}
// (200 with {code?, message, action, completes_at?}: a refusal carries the
// NIP-CW code), GET /api/apps/:id/circle/children; the order and withdraw
// tests use the same routes and /api/test hooks as e2e_lokihub_test.go.
package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	relayclient "github.com/ohstr/nmilat/relay/client"
)

// memberRequestHub is an allowlist circle_hub that serves member requests with
// a rotation delay of delaySecs, with pubHex allowed to join.
func memberRequestHub(t *testing.T, admin *adminClient, pubHex string, delaySecs int) adminCreateAppResponse {
	t.Helper()
	on := true
	opts := circleHubOpts{}.withDefaults()
	req := circleHubRequest("member requests", "allowlist", []string{"circle_wallet"}, opts)
	req.CircleRotationEnabled, req.CircleRotationDelaySecs = &on, delaySecs
	hub := createCircleHubApp(t, admin, req, opts.FundLoki)
	if err := admin.addCircleAllowlistMember(hub.ID, pubHex); err != nil {
		t.Fatalf("allow the member: %v", err)
	}
	return hub
}

// joinMemberHub joins hub from f and returns the wallet's name and pubkey.
func joinMemberHub(t *testing.T, f *fixture, hub adminCreateAppResponse) (string, string) {
	t.Helper()
	out := requireOK(t, "join", f.run("join", "--hub", *hub.CircleHubToken, "--max-amount", "100", "--yes"))
	name, _ := out["wallet"].(string)
	resp, _ := out["response"].(map[string]any)
	wp, _ := resp["wallet_pubkey"].(string)
	if name == "" || wp == "" {
		t.Fatalf("join: no wallet name or wallet_pubkey: %v", out)
	}
	if resp["rotation_delay_secs"] == nil {
		t.Skipf("skipping: this lokihub issues wallets without rotation_delay_secs (member requests not on the stack): %v", resp)
	}
	return name, wp
}

// memberRequestResult is lokihub's answer to a submitted member request.
type memberRequestResult struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	Action      string `json:"action"`
	CompletesAt *int64 `json:"completes_at"`
}

// submitMemberRequest submits the event `circle request` printed, as the host.
func submitMemberRequest(t *testing.T, admin *adminClient, hubID uint, doc map[string]any) memberRequestResult {
	t.Helper()
	ev, err := json.Marshal(doc["event"])
	if err != nil || string(ev) == "null" {
		t.Fatalf("circle request printed no event: %v", doc)
	}
	var r memberRequestResult
	if err := admin.doBody(http.MethodPost, fmt.Sprintf("/api/circle-hubs/%d/member-requests", hubID),
		map[string]json.RawMessage{"event": ev}, &r); err != nil {
		if strings.Contains(err.Error(), "404") {
			t.Skipf("skipping: this lokihub has no member-requests route (%v)", err)
		}
		t.Fatalf("submit member request: %v", err)
	}
	return r
}

// TestE2E_CircleFreezeUnfreeze freezes a wallet with a member request the
// host submits: its key is refused FROZEN (exit 7) until an unfreeze; a
// request submitted twice is REQUEST_REUSED (NIP-CW §Recovering a Lost
// Connection).
func TestE2E_CircleFreezeUnfreeze(t *testing.T) {
	admin := adminOrSkip(t)
	f, pub := newMember(t)
	hub := memberRequestHub(t, admin, pub, 3600)
	name, wp := joinMemberHub(t, f, hub)

	freeze := requireOK(t, "circle request freeze", f.run("circle", "request", "freeze", name))
	if freeze["action"] != "freeze" || freeze["wallet_pubkey"] != wp {
		t.Fatalf("freeze request: %v", freeze)
	}
	if r := submitMemberRequest(t, admin, hub.ID, freeze); r.Code != "" {
		t.Fatalf("freeze refused: %+v", r)
	}
	if r := submitMemberRequest(t, admin, hub.ID, freeze); r.Code != "REQUEST_REUSED" {
		t.Errorf("the same request again: %+v, want REQUEST_REUSED", r)
	}
	requireFailure(t, "budget while frozen", f.run("-c", name, "wallet", "budget"), 7, "auth", "FROZEN")

	unfreeze := requireOK(t, "circle request unfreeze", f.run("circle", "request", "unfreeze", name))
	if r := submitMemberRequest(t, admin, hub.ID, unfreeze); r.Code != "" {
		t.Fatalf("unfreeze refused: %+v", r)
	}
	requireOK(t, "budget after unfreeze", f.run("-c", name, "wallet", "budget"))
}

// TestE2E_CircleRotate_SettleInstallsTheNewKey rotates a wallet to a key made
// by cashctl: the old key is guarded while the rotation is open, the host
// submits the request, and once the delay has passed `cash settle` looks the
// rotation up by the new key and installs it; the wallet answers it.
func TestE2E_CircleRotate_SettleInstallsTheNewKey(t *testing.T) {
	admin := adminOrSkip(t)
	f, pub := newMember(t)
	hub := memberRequestHub(t, admin, pub, 5)
	name, wp := joinMemberHub(t, f, hub)

	rot := requireOK(t, "circle request rotate", f.run("circle", "request", "rotate", name))
	newKey, _ := rot["new_connection_pubkey"].(string)
	if rot["action"] != "rotate" || newKey == "" || rot["wallet_pubkey"] != wp {
		t.Fatalf("rotate request: %v", rot)
	}
	// The old key is not used by accident while the rotation is open.
	requireFailure(t, "budget with a rotation open", f.run("-c", name, "wallet", "budget"), 2, "usage", "")
	r := submitMemberRequest(t, admin, hub.ID, rot)
	if r.Code != "" || r.CompletesAt == nil {
		t.Fatalf("rotate refused: %+v", r)
	}
	// A second rotate while one is pending is ROTATION_PENDING at the Hub;
	// cashctl prints the open one again instead of making a second key.
	if again := requireOK(t, "rotate again", f.run("circle", "request", "rotate", name)); again["new_connection_pubkey"] != newKey {
		t.Errorf("a second rotate made another key: %v", again)
	}
	time.Sleep(time.Until(time.Unix(*r.CompletesAt+1, 0)))

	done := false
	eventually(t, 2*time.Minute, "rotation installed by cash settle", func() bool {
		res := f.run("cash", "settle", "--no-wait", "--yes")
		var out struct {
			Joins []struct {
				Purpose, State, ConnectionPubkey string
			} `json:"joins"`
		}
		_ = json.Unmarshal([]byte(res.Stdout), &out)
		for _, j := range out.Joins {
			if j.Purpose != "rotation" {
				continue
			}
			if j.State == "not_applied" {
				t.Fatalf("the Hub never accepted the rotation: %s", res.Stdout)
			}
			done = done || j.State == "done"
		}
		return done
	})
	requireOK(t, "budget with the new key", f.run("-c", name, "wallet", "budget"))
	show := requireOK(t, "wallet show", f.run("wallet", "show"))
	if pj, _ := show["pending_joins"].([]any); len(pj) != 0 {
		t.Errorf("the rotation is still listed: %v", pj)
	}
}

// TestE2E_CircleWalletDelete_DustBoundary refuses a member deletion above the
// dust threshold before anything is sent, then deletes an empty wallet on its
// Hub with --yes and forgets it here.
func TestE2E_CircleWalletDelete_DustBoundary(t *testing.T) {
	admin := adminOrSkip(t)
	f, pub := newMember(t)
	hub := memberRequestHub(t, admin, pub, 3600)
	name, wp := joinMemberHub(t, f, hub)

	requireFailure(t, "delete without --yes", f.run("wallet", "delete", name), 2, "usage", "")
	out := requireOK(t, "wallet delete", f.run("wallet", "delete", name, "--yes"))
	if out["deleted"] != name || out["wallet_pubkey"] != wp {
		t.Fatalf("wallet delete: %v", out)
	}
	if list := f.run("connect", "list"); strings.Contains(list.Stdout, wp) {
		t.Errorf("the deleted wallet is still registered: %s", list.Stdout)
	}
	// The Hub no longer answers the deleted wallet's key, and cashctl no
	// longer holds it.
	requireFailure(t, "budget of the deleted wallet", f.run("-c", name, "wallet", "budget"), 4, "not_found", "")

	// Above dust: a funded wallet is refused before anything is sent.
	g, pub2 := newMember(t)
	if err := admin.addCircleAllowlistMember(hub.ID, pub2); err != nil {
		t.Fatal(err)
	}
	name2, _ := joinMemberHub(t, g, hub)
	inv, _ := requireOK(t, "invoice", g.run("-c", name2, "wallet", "invoice", "10"))["invoice"].(string)
	payWithPlainWallet(t, setUpPlainWalletWithPayScope(t, admin), inv)
	requireFailure(t, "delete above dust", g.run("wallet", "delete", name2, "--yes"), 5, "conflict", "")
}

// TestE2E_LostReply_OrderKilledAfterSendSettles kills `cash order place`
// right after it sent cash_order; `cash order settle` reads the order with
// cash_lookup, its invoice is paid by the payer, and exactly one bill is
// kept (NIP-CASH §Lost Replies; NIP-CO §Placing an Order).
func TestE2E_LostReply_OrderKilledAfterSendSettles(t *testing.T) {
	admin := hooksOrSkip(t)
	cashorder, _ := openOrderConnection(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	res := f.runWith([]string{"CASHCTL_CRASHPOINT=calls.after_send"}, "", "cash", "order", "place", cashorder, lokiArg(5_000), "--to", "self")
	if res.ExitCode == 0 {
		t.Fatalf("the crashpoint did not fire: %s", res.Stdout)
	}
	var id, inv string
	eventually(t, 3*time.Minute, "lost order recovered by cash order settle", func() bool {
		out := mustDecodeJSON(t, "cash order settle", f.run("cash", "order", "settle").Stdout)
		orders, _ := out["orders"].([]any)
		for _, o := range orders {
			m, _ := o.(map[string]any)
			id, _ = m["order"].(string)
			inv, _ = m["invoice"].(string)
		}
		return id != "" && inv != ""
	})
	if p := admin.hookPayInvoice(t, inv); p.Status != "SUCCEEDED" {
		t.Fatalf("payer could not pay the recovered order's invoice: %+v", p)
	}
	if w := f.run("cash", "order", "watch", id, "--timeout", "3m"); w.ExitCode != 0 {
		t.Fatalf("order watch: exit %d\nstdout: %s\nstderr: %s", w.ExitCode, w.Stdout, w.Stderr)
	}
	o := showOrder(t, f, id)
	if bills, _ := o["bills"].([]any); !orderFinished(o) || len(bills) != 1 {
		t.Fatalf("the recovered order is finished with one bill: %v", o)
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 1 {
		t.Fatalf("one bill held after the lost order: %v", held)
	}
}

// TestE2E_LostReply_OrderKilledBeforeSendIsNeverPlaced kills `cash order
// place` after the write-ahead, before anything was sent: the lookup answers
// unknown once the deadline has passed, the order waits for an explicit
// --replace, and nothing is minted.
func TestE2E_LostReply_OrderKilledBeforeSendIsNeverPlaced(t *testing.T) {
	admin := adminOrSkip(t)
	cashorder, _ := openOrderConnection(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	res := f.runWith([]string{"CASHCTL_CRASHPOINT=calls.after_writeahead"}, "", "cash", "order", "place", cashorder, lokiArg(5_000), "--to", "self")
	if res.ExitCode == 0 {
		t.Fatalf("the crashpoint did not fire: %s", res.Stdout)
	}
	eventually(t, 4*time.Minute, "the never-sent order is stuck for --replace", func() bool {
		f.run("cash", "order", "settle")
		var out struct {
			Calls []struct{ Method, State, Detail string } `json:"calls"`
		}
		_ = json.Unmarshal([]byte(f.run("cash", "settle", "--no-wait", "--yes").Stdout), &out)
		for _, c := range out.Calls {
			if c.Method == "cash_order" && c.State == "stuck" && c.Detail == "confirm_replace" {
				return true
			}
		}
		return false
	})
	if held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any); len(held) != 0 {
		t.Fatalf("an order never sent mints nothing: %v", held)
	}
}

// TestE2E_LostReply_WithdrawKilledAfterSendSettles kills `cash withdraw send`
// right after it sent cash_withdraw; `cash withdraw show` looks the call up,
// recovers the payout, and it is mined to final (NIP-CASH §Lost Replies,
// §Payout States). The bill is never resent.
func TestE2E_LostReply_WithdrawKilledAfterSendSettles(t *testing.T) {
	admin := hooksOrSkip(t)
	hub := withdrawHub(t, admin)
	f, pub := newMember(t)
	f.mustJSON("receive", mintPubkeyTokenFromHub(t, hub, pub, 21_000_000))
	q := f.mustJSON("cash", "withdraw", "quote", admin.hookPayerAddress(t))
	handle, _ := q["quote"].(string)
	if handle == "" {
		t.Fatalf("withdraw quote: no handle: %v", q)
	}
	res := f.runWith([]string{"CASHCTL_CRASHPOINT=calls.after_send"}, "", "cash", "withdraw", "send", "--quote", handle, "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("the crashpoint did not fire: %s", res.Stdout)
	}
	var id string
	eventually(t, 3*time.Minute, "lost withdrawal recovered", func() bool {
		out := mustDecodeJSON(t, "cash withdraw show", f.run("cash", "withdraw", "show").Stdout)
		ws, _ := out["withdrawals"].([]any)
		for _, w := range ws {
			m, _ := w.(map[string]any)
			switch m["state"] {
			case "queued", "late", "broadcast", "confirmed", "final":
				id, _ = m["withdrawal"].(string)
				return true
			}
		}
		return false
	})
	admin.mineUntil(t, 30, "recovered payout final", func() bool {
		return showWithdrawal(t, f, id)["state"] == "final"
	})
	if held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any); len(held) != 0 {
		t.Fatalf("a withdrawn bill is not held (nor resent): %v", held)
	}
}

// payWithPlainWallet pays bolt11 from a plain funded wallet of the node.
func payWithPlainWallet(t *testing.T, payer adminCreateAppResponse, bolt11 string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pairing, err := nip47.ParsePairingURI(payer.PairingUri)
	if err != nil {
		t.Fatalf("parse payer pairing URI: %v", err)
	}
	c, err := relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial payer wallet: %v", err)
	}
	defer c.Close()
	if _, err := c.PayInvoice(ctx, nip47.PayInvoiceParams{Invoice: bolt11}); err != nil {
		t.Fatalf("payer: %v", err)
	}
}
