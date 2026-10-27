//go:build integration

package integration

import (
	"encoding/json"
	"testing"
	"time"

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

// TestFakeHub_ActsOnTheCodeNotTheMessage: the hub answers an error with a
// random message each time; cashctl's classification follows the code alone.
func TestFakeHub_ActsOnTheCodeNotTheMessage(t *testing.T) {
	exits := map[bool]result{}
	for _, random := range []bool{false, true} {
		f, _ := newMember(t)
		hub := fakehub.New(t, func(item transport.Item) *transport.Result { return nil })
		hub.AnswerCode("cash_status", "RESTRICTED")
		if random {
			hub.RandomMessages()
		}
		bill, _ := hub.Mint(25_000, "")
		exits[random] = f.run("receive", bill)
	}
	plain, random := exits[false], exits[true]
	if plain.ExitCode == 0 || random.ExitCode != plain.ExitCode {
		t.Fatalf("exit %d with random messages, %d without; want the same failure", random.ExitCode, plain.ExitCode)
	}
	pe, re := jsonError(t, plain), jsonError(t, random)
	if re.Code != pe.Code || re.NWCCode != pe.NWCCode || pe.NWCCode != "RESTRICTED" {
		t.Errorf("code %q/%q with random messages, %q/%q without; want the same, nwc_code RESTRICTED", re.Code, re.NWCCode, pe.Code, pe.NWCCode)
	}
}

// jsonError decodes a --json failure's stderr.
func jsonError(t *testing.T, res result) cliError {
	t.Helper()
	var e cliError
	if err := json.Unmarshal([]byte(res.Stderr), &e); err != nil {
		t.Fatalf("stderr is not a --json error: %v\n%s", err, res.Stderr)
	}
	return e
}

// TestFakeHub_DropRepliesIsIndeterminate: the transfer reaches the hub and its
// reply is lost. cashctl must say whether it happened is unknown, and never
// send it again.
func TestFakeHub_DropRepliesIsIndeterminate(t *testing.T) {
	f, pub := newMember(t)
	hub := fakehub.New(t, func(item transport.Item) *transport.Result {
		if item.Method == "cash_status" {
			return fakehub.StatusReply(item, "pubkey", pub, 25_000)
		}
		return nil
	})
	bill, _ := hub.Mint(25_000, "")
	id := entryID(t, requireOK(t, "receive", f.run("receive", bill))["entry"])

	hub.DropReplies("cash_transfer", -1)
	_, other := newIdentity(t)
	start := time.Now()
	requireFailure(t, "transfer with its reply lost", f.run("transfer", "10", other, "--token", id, "--yes"), 8, "indeterminate", "")
	t.Logf("gave up after %v", time.Since(start).Round(time.Second))
	if n := len(hub.ItemsFor("cash_transfer")); n != 1 {
		t.Errorf("the transfer reached the hub %d times, want exactly once", n)
	}
}
