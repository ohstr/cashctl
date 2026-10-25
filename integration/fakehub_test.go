//go:build integration

package integration

import (
	"testing"

	"github.com/ohstr/nmilat/nipcash/transport"

	"github.com/ohstr/cashctl/integration/fakehub"
)

// TestFakeHub_HonestReceive is the harness's own check: an honest answer
// from the fake hub is received like a real Hub's.
func TestFakeHub_HonestReceive(t *testing.T) {
	f, pub := newMember(t)
	hub := fakehub.New(t, func(item transport.Item) *transport.Result {
		return fakehub.StatusReply(item, "pubkey", pub, 25_000)
	})
	bill, _ := hub.Mint(25_000, "")
	out := requireOK(t, "receive from the fake hub", f.run("receive", bill))
	entry, _ := out["entry"].(map[string]any)
	if got, _ := entry["amount_millis"].(float64); got != 25_000 {
		t.Errorf("received amount = %v, want 25000: %v", entry["amount_millis"], out)
	}
	if n := len(hub.ItemsFor("cash_status")); n == 0 {
		t.Error("the fake hub saw no cash_status: receive did not go over the private transport")
	}
}
