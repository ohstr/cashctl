//go:build integration

// circle_private_join_test.go checks NIP-CW §Private Join end to end: when the
// Hub offers it, cashctl's join travels over the private transport, which a
// listener on the Hub's inbox confirms independently of cashctl's own report.
// Every join is private: a Hub whose announcement can't be found gets no join
// at all, covered by the unit tests in cmd/circle_join_test.go.
package integration

import (
	"context"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
)

// circleHubInbox resolves a circle hub's private-transport inbox the way a
// joining client does: get_info names the node identity, whose announcement
// names the inbox. Returns the relay to listen on as well.
func circleHubInbox(t *testing.T, pairingURI string) (relay, inbox string) {
	t.Helper()
	pairing, err := nip47.ParsePairingURI(pairingURI)
	if err != nil {
		t.Fatalf("parse hub pairing uri: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	nwc, err := relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	defer nwc.Close()
	info, err := nwc.GetInfo(ctx)
	if err != nil {
		t.Fatalf("hub get_info: %v", err)
	}
	if info.CircleWallet == nil || info.CircleWallet.Identity == "" {
		t.Fatalf("hub get_info names no private-join identity (circle_wallet=%+v): this Hub does not offer private join", info.CircleWallet)
	}
	session, err := (&nipcashclient.Client{}).NewBatchSession(ctx, info.CircleWallet.Identity, pairing.RelayURLs)
	if err != nil {
		t.Fatalf("resolve the hub's private-transport announcement: %v", err)
	}
	if session.Inbox() == "" {
		t.Fatal("the announcement names no inbox")
	}
	return pairing.RelayURLs[0], session.Inbox()
}

// TestCircleJoin_PrivateWhenOffered joins a Hub that offers private join,
// through the `join` shortcut and the canonical `circle join` (every join is
// private), and confirms each request reached the Hub's private inbox.
func TestCircleJoin_PrivateWhenOffered(t *testing.T) {
	admin := adminOrSkip(t)
	byDefault, byDefaultPub := newMember(t)
	required, requiredPub := newMember(t)

	opts := circleHubOpts{FundLoki: 1000}.withDefaults()
	hub := createCircleHubApp(t, admin, circleHubRequest("private join circle_hub", "allowlist", []string{"circle_wallet", "get_info"}, opts), opts.FundLoki)
	for _, pub := range []string{byDefaultPub, requiredPub} {
		if err := admin.addCircleAllowlistMember(hub.ID, pub); err != nil {
			t.Fatalf("authorize member: %v", err)
		}
	}
	relay, inbox := circleHubInbox(t, hub.PairingUri)

	for _, tc := range []struct {
		name string
		f    *fixture
		args []string
	}{
		{"join", byDefault, []string{"join", *hub.CircleHubToken, "20"}},
		{"circle join", required, []string{"circle", "join", *hub.CircleHubToken, "20"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := countPrivateRequests(t, relay, inbox)
			res := tc.f.run(tc.args...)
			seen := stop()
			if res.ExitCode != 0 {
				t.Fatalf("join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
			}
			joined := mustDecodeJSON(t, "join", res.Stdout)
			for _, gone := range []string{"private", "fallback_reason"} {
				if _, ok := joined[gone]; ok {
					t.Errorf("--json still carries %q: every join is private", gone)
				}
			}
			if seen == 0 {
				t.Errorf("no private request reached the Hub's inbox %s, though cashctl reported a private join", inbox)
			}
			tc.f.mustJSON("wallet", "get-info")
			requirePendingJoinsEmpty(t, tc.name, tc.f)
		})
	}
}
