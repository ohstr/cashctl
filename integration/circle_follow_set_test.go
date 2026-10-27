//go:build integration

// circle_follow_set_test.go joins follow_set circles (NIP-CW §Membership,
// §Hidden Members) through the cashctl binary: who gets in, who is refused
// and with which exit code, and what a republished set does to members who
// already hold a wallet. How the Hub reads and caches the set is lokihub's
// own suite's job; this file asserts only what a member sees.
package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"
)

// kindFollowSet is NIP-51's follow set, the list a follow_set circle reads.
const kindFollowSet = 30000

// followSetJoinAmount is what every member here asks for, small enough that
// several members fit under followSetHubOpts' funding.
const followSetJoinAmount = "10"

var followSetHubOpts = circleHubOpts{FundLoki: 1000}

// followSetHubScopes adds get_info to the join scope, so `decode --check`
// sees the circle's policy and whether it offers private join.
var followSetHubScopes = []string{"circle_wallet", "get_info"}

func randomFollowSetD(t *testing.T) string {
	t.Helper()
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return "e2e-" + hex.EncodeToString(b)
}

// setUpManagedFollowSetHub creates a follow_set circle_hub whose set the
// node identity publishes, starting from set.
func setUpManagedFollowSetHub(t *testing.T, admin *adminClient, set adminFollowSet) (hub adminCreateAppResponse, identityID uint) {
	t.Helper()
	opts := followSetHubOpts.withDefaults()
	req := circleHubRequest("follow_set circle_hub", "follow_set", followSetHubScopes, opts)
	req.FollowSetD = randomFollowSetD(t)
	req.FollowSet = &set
	hub = createCircleHubApp(t, admin, req, opts.FundLoki)
	identityID, err := admin.getCircleIdentityID(hub.ID)
	if err != nil {
		t.Fatalf("resolve circle identity: %v", err)
	}
	return hub, identityID
}

// publishProviderFollowSet signs a follow set as providerPriv and publishes
// it to relayURL, the relay the Hub reads follow sets from.
func publishProviderFollowSet(t *testing.T, relayURL, providerPriv, d string, createdAt uint64, members ...string) {
	t.Helper()
	tags := [][]string{{"d", d}}
	for _, m := range members {
		tags = append(tags, []string{"p", m})
	}
	ev := nip01.NewEvent(kindFollowSet, "", tags...)
	ev.CreatedAt = createdAt
	if err := ev.Sign(providerPriv); err != nil {
		t.Fatalf("sign follow set: %v", err)
	}
	u, err := url.Parse(relayURL)
	if err != nil {
		t.Fatalf("parse relay url %q: %v", relayURL, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	conn, err := relayclient.Connect(ctx, u)
	if err != nil {
		t.Fatalf("connect to %s: %v", relayURL, err)
	}
	defer conn.Close()
	ok, err := conn.Publish(ctx, ev)
	if err != nil || !ok.Accepted {
		t.Fatalf("publish follow set to %s: ok=%+v err=%v", relayURL, ok, err)
	}
}

// refreshUntilResolved asks the Hub to re-read its follow set, retrying while
// the relay has not served the newly published version yet (503).
func refreshUntilResolved(t *testing.T, admin *adminClient, hubID uint) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := admin.refreshCircle(hubID)
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("refresh follow set of hub %d: %v", hubID, err)
		}
		time.Sleep(2 * time.Second)
	}
}

func joinFollowSet(f *fixture, hub adminCreateAppResponse, extra ...string) result {
	return f.run(append([]string{"join", *hub.CircleHubToken, followSetJoinAmount}, extra...)...)
}

func requirePendingJoinsEmpty(t *testing.T, step string, f *fixture) {
	t.Helper()
	if pending, _ := f.mustJSON("wallet", "show")["pending_joins"].([]any); len(pending) != 0 {
		t.Errorf("%s: pending_joins = %v, want none (a refused join creates no wallet)", step, pending)
	}
}

// TestFollowSetCircle_ManagedSet runs a circle whose set the Hub publishes
// itself: a public and a hidden member join, an outsider is refused, a
// republish drops a member who already holds a wallet, and an empty set
// closes the circle to someone who was a member until then.
func TestFollowSetCircle_ManagedSet(t *testing.T) {
	admin := adminOrSkip(t)
	public, publicPub := newMember(t)
	hidden, hiddenPub := newMember(t)
	later, laterPub := newMember(t)
	outsider, _ := newMember(t)

	const title = "cashctl e2e follow set"
	hub, identityID := setUpManagedFollowSetHub(t, admin, adminFollowSet{
		Title:         title,
		PublicMembers: []string{publicPub, laterPub},
		HiddenMembers: []string{hiddenPub},
	})

	t.Run("decode shows the set's title and its policy", func(t *testing.T) {
		dec := public.mustJSON("decode", *hub.CircleHubToken, "--check")
		if label, _ := dec["label"].(string); label != title {
			t.Errorf("label = %q, want the set's title %q", label, title)
		}
		check, _ := dec["check"].(map[string]any)
		if policy, _ := check["circle_policy"].(string); policy != "follow_set" {
			t.Errorf("check.circle_policy = %q, want follow_set (check: %v)", policy, check)
		}
		if offered, _ := check["private_join"].(bool); !offered {
			t.Errorf("check.private_join = %v, want true: a hidden member can only join privately", check["private_join"])
		}
	})

	t.Run("public member joins", func(t *testing.T) {
		res := joinFollowSet(public, hub)
		if res.ExitCode != 0 {
			t.Fatalf("join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
		}
	})

	t.Run("hidden member joins privately", func(t *testing.T) {
		hidden.mustJSON("join", *hub.CircleHubToken, followSetJoinAmount)
		hidden.mustJSON("wallet", "get-info")
	})

	t.Run("outsider is refused", func(t *testing.T) {
		requireFailure(t, "outsider join", joinFollowSet(outsider, hub), 7, "auth", "RESTRICTED")
		requirePendingJoinsEmpty(t, "outsider", outsider)
	})

	t.Run("republishing without a member refuses them but keeps their wallet", func(t *testing.T) {
		if err := admin.publishFollowSet(identityID, adminFollowSet{
			Title: title, PublicMembers: []string{publicPub, laterPub},
		}); err != nil {
			t.Fatalf("republish follow set: %v", err)
		}
		// Membership is checked before the one-wallet rule: the dropped
		// member is told they are not a member, not that they hold a wallet.
		requireFailure(t, "dropped member re-joins", joinFollowSet(hidden, hub), 7, "auth", "RESTRICTED")
		requirePendingJoinsEmpty(t, "dropped member", hidden)
		// Dropping a member does not revoke the wallet they already hold.
		hidden.mustJSON("wallet", "get-info")
		// A member who stays gets past membership and hits the one-wallet rule.
		requireFailure(t, "remaining member re-joins", joinFollowSet(public, hub), 9, "exists", "WALLET_EXISTS")
	})

	t.Run("an empty set closes the circle", func(t *testing.T) {
		if err := admin.publishFollowSet(identityID, adminFollowSet{Title: title}); err != nil {
			t.Fatalf("publish empty follow set: %v", err)
		}
		requireFailure(t, "former member joins a closed circle", joinFollowSet(later, hub), 7, "auth", "RESTRICTED")
	})
}

// TestFollowSetCircle_ExternalProvider reads a set someone else authors: a
// set the Hub cannot find yet answers NOT_READY (retryable, so cashctl's
// network code), and a newer version replaces the old one once the Hub has
// read it.
func TestFollowSetCircle_ExternalProvider(t *testing.T) {
	admin := adminOrSkip(t)
	info, err := admin.getInfo()
	if err != nil || info.GeneralRelay == "" {
		t.Skipf("skipping: no general relay to publish a follow set to (info=%+v err=%v)", info, err)
	}
	// The general relay setting may list several, comma-separated.
	relayURL := strings.TrimSpace(strings.Split(info.GeneralRelay, ",")[0])

	member, memberPub := newMember(t)
	newcomer, newcomerPub := newMember(t)
	providerPriv := randomHex32(t)
	providerPub, err := utils.GetPublicKey(providerPriv)
	if err != nil {
		t.Fatalf("derive provider pubkey: %v", err)
	}
	d := randomFollowSetD(t)

	opts := followSetHubOpts.withDefaults()
	req := circleHubRequest("external follow_set circle_hub", "follow_set", followSetHubScopes, opts)
	req.ProviderPubkey = providerPub
	req.FollowSetD = d
	hub := createCircleHubApp(t, admin, req, opts.FundLoki)

	t.Run("an unpublished set is not ready, and retryable", func(t *testing.T) {
		e := requireFailure(t, "join before the set exists", joinFollowSet(member, hub), 6, "network", "NOT_READY")
		if !e.Retryable {
			t.Errorf("retryable = false, want true: the Hub only has not found the set yet")
		}
		requirePendingJoinsEmpty(t, "not ready", member)
	})

	first := uint64(time.Now().Unix())
	t.Run("once published and read, a member joins", func(t *testing.T) {
		publishProviderFollowSet(t, relayURL, providerPriv, d, first, memberPub)
		refreshUntilResolved(t, admin, hub.ID)
		res := joinFollowSet(member, hub)
		if res.ExitCode != 0 {
			t.Fatalf("join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
		}
	})

	t.Run("a newer version replaces the membership", func(t *testing.T) {
		publishProviderFollowSet(t, relayURL, providerPriv, d, first+1, newcomerPub)
		refreshUntilResolved(t, admin, hub.ID)
		requireFailure(t, "dropped member re-joins", joinFollowSet(member, hub), 7, "auth", "RESTRICTED")
		res := joinFollowSet(newcomer, hub)
		if res.ExitCode != 0 {
			t.Fatalf("newcomer join: exit %d\nstderr: %s", res.ExitCode, res.Stderr)
		}
	})
}
