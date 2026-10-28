//go:build integration

// e2e_lokihub_test.go drives the parts of cashctl the rest of this suite
// never reaches against lokihub's regtest dev stack: orders paid by the
// stack's payer (Lightning and on-chain), on-chain withdrawals mined to
// final and cancelled, lost replies made real by killing cashctl right after
// it sent a call (a crashpoints build) and settled with `cash settle`, and an
// offer listing found with `cash discover` and joined.
//
// Every test needs the regtest stack's test hooks (hooksOrSkip), except the
// lost-reply ones, which need only the admin API and the crashpoints binary
// the harness always builds.
package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// e2eOrderTerms are the terms the order tests sell under: a Lightning window
// long enough for the payer, on-chain at one confirmation.
func e2eOrderTerms() map[string]any {
	return map[string]any{
		"terms": map[string]any{
			"lightning": map[string]any{"min_millis": 1000, "max_millis": 1_000_000, "fee_base": 1000, "fee_ppm": 1000, "window": 600},
			"onchain": map[string]any{"min_millis": 10_000, "max_millis": 5_000_000, "fee_base": 21000, "fee_ppm": 2000, "window": 3600,
				"confirmations": []map[string]any{{"n": 1}}},
			"bill": map[string]any{"redeem_fee_base": 0, "redeem_fee_ppm": 0, "min_transfer_millis": 0},
		},
		"openCeilingLightningMillis": 5_000_000,
		"openCeilingOnchainMillis":   5_000_000,
		"openOnchainMax":             5,
	}
}

// openOrderConnection sets order terms on a fresh Cash Hub and returns a new
// Order connection's signed cashorder1 string, with the hub's app id.
func openOrderConnection(t *testing.T, admin *adminClient) (string, uint) {
	t.Helper()
	hub := setUpCashHub(t, admin)
	admin.mustDo(t, http.MethodPut, hubPath(hub.ID, "order-terms"), e2eOrderTerms(), nil)
	var conn struct {
		ID               uint   `json:"id"`
		ConnectionString string `json:"connectionString"`
	}
	admin.mustDo(t, http.MethodPost, hubPath(hub.ID, "order-connections"), map[string]string{"label": "cashctl e2e"}, &conn)
	if !strings.HasPrefix(conn.ConnectionString, "cashorder1") {
		t.Fatalf("order connection: no cashorder1 string: %+v", conn)
	}
	return conn.ConnectionString, hub.ID
}

func showOrder(t *testing.T, f *fixture, id string) map[string]any {
	t.Helper()
	out := f.mustJSON("cash", "order", "show", id)
	if orders, ok := out["orders"].([]any); ok && len(orders) > 0 {
		if o, ok := orders[0].(map[string]any); ok {
			return o
		}
	}
	return out
}

func orderFinished(o map[string]any) bool {
	done, _ := o["finished"].(bool)
	return done
}

// TestE2E_OrderLightning_PaidByPayerMintedOnce places a Lightning order over
// a cashorder1 string, pays its invoice from the payer node and watches the
// order until it is finished with one bill (NIP-CO §Paying and Minting).
func TestE2E_OrderLightning_PaidByPayerMintedOnce(t *testing.T) {
	admin := hooksOrSkip(t)
	cashorder, _ := openOrderConnection(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	placed := f.mustJSON("cash", "order", "place", cashorder, lokiArg(5_000), "--to", "self")
	id, _ := placed["order"].(string)
	inv, _ := placed["invoice"].(string)
	if id == "" || inv == "" {
		t.Fatalf("order place: no order or invoice: %v", placed)
	}
	if p := admin.hookPayInvoice(t, inv); p.Status != "SUCCEEDED" {
		t.Fatalf("payer could not pay the order invoice: %+v", p)
	}
	res := f.run("cash", "order", "watch", id, "--timeout", "3m")
	if res.ExitCode != 0 {
		t.Fatalf("order watch: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	o := showOrder(t, f, id)
	if !orderFinished(o) {
		t.Fatalf("order not finished after payment: %v", o)
	}
	bills, _ := o["bills"].([]any)
	if len(bills) != 1 {
		t.Fatalf("a paid Lightning order mints exactly one bill, got %d: %v", len(bills), o)
	}
	// A second watch reads the same order; nothing is minted twice.
	again := showOrder(t, f, id)
	if b2, _ := again["bills"].([]any); len(b2) != 1 {
		t.Fatalf("order re-read shows %d bills", len(b2))
	}
}

// TestE2E_OrderOnchain_PaidAndConfirmed places an on-chain order, pays its
// address from the payer in one output (rounded up to whole loki: an
// overpayment mints more, never a refund) and mines until it is minted.
func TestE2E_OrderOnchain_PaidAndConfirmed(t *testing.T) {
	admin := hooksOrSkip(t)
	cashorder, _ := openOrderConnection(t, admin)
	f := newFixture(t)
	f.mustJSON("wallet", "init")

	// Large enough that its single output is not dust on the chain.
	placed := f.mustJSON("cash", "order", "place", cashorder, lokiArg(1_000_000), "--onchain", "--to", "self")
	id, _ := placed["order"].(string)
	addr, _ := placed["address"].(string)
	pay := uint64(mloki(placed, "pay_millis"))
	if id == "" || addr == "" || pay == 0 {
		t.Fatalf("on-chain order place: no order, address or pay_millis: %v", placed)
	}
	pay = (pay + 999) / 1000 * 1000
	txid := admin.hookPayAddress(t, addr, pay)
	if txid == "" {
		t.Fatal("payer pay-address: no txid")
	}
	// An on-chain order stays open until it expires or is closed and may gain
	// more bills (NIP-CO §Client Behavior), so the wait is for the bill.
	// lokihub scans the chain for order payments every 30 s: mine for longer.
	admin.mineUntil(t, 40, "on-chain order minted", func() bool {
		b, _ := showOrder(t, f, id)["bills"].([]any)
		return len(b) > 0
	})
	o := showOrder(t, f, id)
	bills, _ := o["bills"].([]any)
	if len(bills) != 1 {
		t.Fatalf("one on-chain output mints one bill, got %d: %v", len(bills), o)
	}
	if orderFinished(o) {
		t.Errorf("an open on-chain order is not finished by its first bill: %v", o)
	}
	if b, _ := bills[0].(map[string]any); b["outpoint"] == nil || !strings.HasPrefix(b["outpoint"].(string), txid) {
		t.Errorf("the bill names the paying outpoint %s: %v", txid, b)
	}
}

// withdrawHub is a Cash Hub with on-chain withdrawals offered, or a skip when
// the node is not capable (WP-23/24 not on the stack, or no on-chain funds).
func withdrawHub(t *testing.T, admin *adminClient) adminCreateAppResponse {
	t.Helper()
	hub := setUpCashHubOpts(t, admin, cashHubOpts{PerWalletMaxMloki: 50_000_000, FundLoki: 30_000})
	if err := admin.doBody(http.MethodPut, "/api/cash-withdraw/node", map[string]bool{"enabled": true}, nil); err != nil {
		t.Skipf("skipping: the Hub has no on-chain withdrawals (%v)", err)
	}
	var view struct {
		Offered bool `json:"offered"`
		Capable bool `json:"capable"`
	}
	admin.mustDo(t, http.MethodPut, hubPath(hub.ID, "withdraw-terms"),
		map[string]any{"enabled": true, "fee_base_millis": 1000, "fee_ppm": 1000}, &view)
	if !view.Offered {
		t.Skipf("skipping: withdrawals not offered on this Hub (capable=%t)", view.Capable)
	}
	return hub
}

func showWithdrawal(t *testing.T, f *fixture, id string) map[string]any {
	t.Helper()
	out := f.mustJSON("cash", "withdraw", "show", id)
	if ws, ok := out["withdrawals"].([]any); ok && len(ws) > 0 {
		if w, ok := ws[0].(map[string]any); ok {
			return w
		}
	}
	return out
}

// sendWithdrawal receives a fresh 21,000-loki bill into f, quotes it to a
// payer address and sends the quote, returning the withdrawal id.
func sendWithdrawal(t *testing.T, admin *adminClient, hub adminCreateAppResponse, f *fixture, pubHex string) string {
	t.Helper()
	token := mintPubkeyTokenFromHub(t, hub, pubHex, 21_000_000)
	f.mustJSON("receive", token)
	addr := admin.hookPayerAddress(t)
	q := f.mustJSON("cash", "withdraw", "quote", addr)
	handle, _ := q["quote"].(string)
	if handle == "" {
		t.Fatalf("withdraw quote: no handle: %v", q)
	}
	sent := f.mustJSON("cash", "withdraw", "send", "--quote", handle, "--yes")
	id, _ := sent["withdrawal"].(string)
	if id == "" {
		t.Fatalf("withdraw send: no withdrawal id: %v", sent)
	}
	return id
}

// TestE2E_Withdraw_MinedToFinal withdraws a bill on-chain and mines until the
// payout is final (NIP-CASH §Payout States), then checks the paying tx is on
// the chain node.
func TestE2E_Withdraw_MinedToFinal(t *testing.T) {
	admin := hooksOrSkip(t)
	hub := withdrawHub(t, admin)
	f, pub := newMember(t)
	id := sendWithdrawal(t, admin, hub, f, pub)

	admin.mineUntil(t, 30, "payout final", func() bool {
		return showWithdrawal(t, f, id)["state"] == "final"
	})
	w := showWithdrawal(t, f, id)
	txid, _ := w["txid"].(string)
	if txid == "" {
		t.Fatalf("a final payout names its txid: %v", w)
	}
	if tx, ok := admin.hookTx(t, txid); !ok || tx.Depth < 1 {
		t.Fatalf("payout tx %s not confirmed on the chain node (%+v)", txid, tx)
	}
}

// TestE2E_Withdraw_CancelBeforeBroadcastRemints withholds nothing: it asks to
// cancel at once. A Hub that already broadcast refuses (a skip: the race is
// the Hub's to win); one that cancels re-mints the bill, which cashctl holds
// again once the cancel is final.
func TestE2E_Withdraw_CancelBeforeBroadcastRemints(t *testing.T) {
	admin := hooksOrSkip(t)
	hub := withdrawHub(t, admin)
	f, pub := newMember(t)
	id := sendWithdrawal(t, admin, hub, f, pub)

	res := f.run("cash", "withdraw", "cancel", id, "--yes")
	if res.ExitCode != 0 {
		t.Skipf("the Hub did not cancel (exit %d, already broadcast?): %s", res.ExitCode, res.Stderr)
	}
	admin.mineUntil(t, 30, "cancelled payout settled", func() bool {
		w := showWithdrawal(t, f, id)
		done, _ := w["finished"].(bool)
		return w["state"] == "cancelled" && done
	})
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) == 0 {
		t.Fatal("a cancelled withdrawal's re-minted bill is held again")
	}
}

// TestE2E_Withdraw_WithheldPayoutStaysBroadcast withholds the payout from blocks,
// so the payout stays broadcast while blocks are mined, then lets it confirm.
// (A true reorg needs the chain node's invalidateblock, which the glue's
// chain helper drives; see the scenario matrix.)
func TestE2E_Withdraw_WithheldPayoutStaysBroadcast(t *testing.T) {
	admin := hooksOrSkip(t)
	hub := withdrawHub(t, admin)
	f, pub := newMember(t)
	id := sendWithdrawal(t, admin, hub, f, pub)

	var txid string
	eventually(t, 3*time.Minute, "payout broadcast", func() bool {
		w := showWithdrawal(t, f, id)
		txid, _ = w["txid"].(string)
		return txid != ""
	})
	admin.hookWithhold(t, txid, true)
	t.Cleanup(func() { admin.hookWithhold(t, txid, false) })
	admin.hookMine(t, 3)
	if st := showWithdrawal(t, f, id)["state"]; st == "confirmed" || st == "final" {
		t.Fatalf("a withheld payout cannot confirm, got %v", st)
	}
	admin.hookWithhold(t, txid, false)
	admin.mineUntil(t, 30, "payout final after release", func() bool {
		return showWithdrawal(t, f, id)["state"] == "final"
	})
}

// TestE2E_LostReply_TransferKilledAfterSendSettles kills cashctl right after
// it published a cash_transfer, so its reply is lost for good; `cash settle`
// then reads the call with cash_lookup and applies the kept result (NIP-CASH
// §Lost Replies).
func TestE2E_LostReply_TransferKilledAfterSendSettles(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	token := mintPubkeyTokenFromHub(t, hub, pub, 500_000)
	f.mustJSON("receive", token)

	target := fakeHex32(t)
	res := f.runWith([]string{"CASHCTL_CRASHPOINT=calls.after_send"}, "",
		"transfer", "--to", "pubkey:"+target, "--amount", "200", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("the crashpoint did not fire: %s", res.Stdout)
	}
	settled := f.run("cash", "settle", "--wait", "--yes")
	if settled.ExitCode != 0 {
		t.Fatalf("cash settle: exit %d\nstdout: %s\nstderr: %s", settled.ExitCode, settled.Stdout, settled.Stderr)
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	found := false
	for _, h := range held {
		if e, _ := h.(map[string]any); uint64(mloki(e, "amount_millis")) == 300_000 {
			found = true
		}
	}
	if !found {
		t.Fatalf("after settle the transfer's 300-loki remainder is held: %v", held)
	}
	if again := f.run("cash", "settle", "--no-wait", "--yes"); again.ExitCode != 0 {
		t.Fatalf("nothing is left open after settle: exit %d %s", again.ExitCode, again.Stdout)
	}
}

// TestE2E_LostReply_TransferKilledBeforeSendStaysHeld kills cashctl after it
// wrote the call ahead but before it sent it: the lookup answers that the Hub
// never saw it once the deadline passes, so `cash settle` places it again
// under its derived id (transfers and merges, at most three times), and the
// transfer happens exactly once: the 300-loki remainder is held.
func TestE2E_LostReply_TransferKilledBeforeSendStaysHeld(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	token := mintPubkeyTokenFromHub(t, hub, pub, 400_000)
	f.mustJSON("receive", token)

	res := f.runWith([]string{"CASHCTL_CRASHPOINT=calls.after_writeahead"}, "",
		"transfer", "--to", "pubkey:"+fakeHex32(t), "--amount", "100", "--yes")
	if res.ExitCode == 0 {
		t.Fatalf("the crashpoint did not fire: %s", res.Stdout)
	}
	settled := f.run("cash", "settle", "--wait", "--yes")
	if settled.ExitCode != 0 {
		t.Fatalf("cash settle: exit %d\nstdout: %s\nstderr: %s", settled.ExitCode, settled.Stdout, settled.Stderr)
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 1 || uint64(mloki(held[0].(map[string]any), "amount_millis")) != 300_000 {
		t.Fatalf("a call never sent is placed again once, leaving the 300-loki remainder held: %v", held)
	}
	if again := f.run("cash", "settle", "--no-wait", "--yes"); again.ExitCode != 0 {
		t.Fatalf("nothing is left open after settle: exit %d %s", again.ExitCode, again.Stdout)
	}
}

// TestE2E_LostReply_MintKilledAfterSendSettles kills `cash mint` after it
// sent mint_cash; `cash settle` finds the mint done and keeps the bill.
func TestE2E_LostReply_MintKilledAfterSendSettles(t *testing.T) {
	admin := adminOrSkip(t)
	hub := setUpCashHub(t, admin)
	f, pub := newMember(t)
	env := []string{"CASHCTL_CASH_HUB=" + *hub.CashHubToken, "CASHCTL_CRASHPOINT=calls.after_send"}
	// `cash mint --to` takes the wallet's own key for a bill held here (there
	// is no "self" shorthand on mint); a crash point kills with SIGKILL.
	if res := f.runWith(env, "", "cash", "mint", lokiArg(25_000), "--to", pub); res.ExitCode != 137 && res.ExitCode != -1 {
		t.Fatalf("the crashpoint did not fire: exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	settled := f.runWith([]string{"CASHCTL_CASH_HUB=" + *hub.CashHubToken}, "", "cash", "settle", "--wait", "--yes")
	if settled.ExitCode != 0 {
		t.Fatalf("cash settle: exit %d\nstdout: %s\nstderr: %s", settled.ExitCode, settled.Stdout, settled.Stderr)
	}
	held, _ := f.mustJSON("wallet", "show")["held_tokens"].([]any)
	if len(held) != 1 || uint64(mloki(held[0].(map[string]any), "amount_millis")) != 25_000 {
		t.Fatalf("the lost mint's bill is held once settled: %v", held)
	}
}

// TestE2E_Discover_CircleListingJoined lists a circle from the admin API on
// the Hub's own relay, finds it with `cash discover list`, and joins it with
// `cash discover open` (NIP-CASH §Listing Offers; NIP-CW §Private Join).
func TestE2E_Discover_CircleListingJoined(t *testing.T) {
	admin := adminOrSkip(t)
	cfg, _ := LoadConfig("")
	if cfg.RelayURL == "" {
		t.Skip("skipping: relay_url not configured")
	}
	f, pub := newMember(t)
	hub := setUpCircleHub(t, admin, pub)

	var before struct {
		Configured []string `json:"configured"`
	}
	if err := admin.doBody(http.MethodGet, "/api/listing-relays", nil, &before); err != nil {
		t.Skipf("skipping: the Hub publishes no listings (%v)", err)
	}
	admin.mustDo(t, http.MethodPut, "/api/listing-relays", map[string]any{"relays": []string{cfg.RelayURL}}, nil)
	t.Cleanup(func() {
		_ = admin.doBody(http.MethodPut, "/api/listing-relays", map[string]any{"relays": before.Configured}, nil)
	})
	var l struct {
		ID      uint   `json:"id"`
		Address string `json:"address"`
	}
	title := "cashctl e2e circle " + time.Now().Format("150405")
	admin.mustDo(t, http.MethodPost, "/api/listings", map[string]any{"circle_hub_app_id": hub.ID, "title": title, "content": "e2e"}, &l)
	t.Cleanup(func() { _ = admin.doBody(http.MethodPost, "/api/listings/"+uitoa(l.ID)+"/unlist", nil, nil) })

	f.mustJSON("cash", "discover", "relays", "add", cfg.RelayURL)
	var offer string
	eventually(t, time.Minute, "listing on the relay", func() bool {
		out := f.mustJSON("cash", "discover", "list", "--type", "circle", "--test-networks")
		offers, _ := out["offers"].([]any)
		for _, o := range offers {
			if m, _ := o.(map[string]any); m["title"] == title {
				offer, _ = m["offer"].(string)
			}
		}
		return offer != ""
	})
	res := f.run("cash", "discover", "open", offer, "50", "--test-networks", "--yes")
	if res.ExitCode != 0 {
		t.Fatalf("discover open (join): exit %d\nstdout: %s\nstderr: %s", res.ExitCode, res.Stdout, res.Stderr)
	}
	var joined map[string]any
	if err := json.Unmarshal([]byte(res.Stdout), &joined); err != nil {
		t.Fatalf("discover open: %v: %s", err, res.Stdout)
	}
	children, err := admin.listCircleChildren(hub.ID)
	if err != nil || len(children) != 1 {
		t.Fatalf("the join made one Circle Wallet on the Hub: %v %v", children, err)
	}
}

func uitoa(n uint) string {
	b, _ := json.Marshal(n)
	return string(b)
}
