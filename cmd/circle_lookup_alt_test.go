package cmd

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/testmint"
)

// writeOpenJoin writes an open join of the circle with TLV 0 hub, joined with
// conn, its deadline long past.
func writeOpenJoin(t *testing.T, hub, conn, connPub string) config.Join {
	t.Helper()
	j := config.Join{ConnectionPubkey: connPub, ConnectionSecret: testmint.Hex32(t), HubPubkey: hub,
		MemberPubkey: testMemberPub(t), HubConnection: conn, HubIdentity: strings.Repeat("cd", 32), Chain: "flokicoin",
		Deadline: time.Now().Unix() - joinUnansweredAfter - 10, DeviceID: "d", State: config.JoinWritten}
	if err := config.WriteJoin(j); err != nil {
		t.Fatal(err)
	}
	got, _, _ := config.FindJoin(connPub)
	return got
}

// R-CW-21: a join whose lookups all went unanswered past deadline + 360 s is
// looked up once over another string of the same circle (same TLV 0); with
// none, or once that was tried too, it is shown lookup_unanswered.
func TestSettleJoin_TriesAnotherStringOfTheCircleOnce(t *testing.T) {
	f := &fakeCircleHub{}
	setUpCircleHub(t, f)
	hub := strings.Repeat("ab", 32)
	var dialled []string
	prev := dialCircleHubFn
	dialCircleHubFn = func(_ context.Context, s string) (circleHub, error) {
		dialled = append(dialled, s)
		return f, nil
	}
	t.Cleanup(func() { dialCircleHubFn = prev })
	answerFrom := ""
	f.lookup = func(pj nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) {
		if dialled[len(dialled)-1] == answerFrom {
			return nipcwclient.JoinAction{Kind: nipcwclient.JoinClose, Response: walletResponse(pj)}, nil
		}
		return nipcwclient.JoinAction{Kind: nipcwclient.JoinWait}, nil // omitted
	}

	// Another string of the same circle is held (an earlier join over it).
	other := "circlehub1other"
	writeOpenJoin(t, hub, other, testmint.Hex32(t))
	_ = config.SetJoinState(mustOnlyOther(t, other), config.JoinDone, "", 0)
	j := writeOpenJoin(t, hub, "circlehub1revoked", testmint.Hex32(t))
	answerFrom = other
	out := settleOneJoin(newCashSettleCmd(), j, mustCred(t))
	if len(dialled) != 1 || dialled[0] != other || out.Join.State != config.JoinDone {
		t.Fatalf("dialled %v, state %s: want the other string once, and the wallet", dialled, out.Join.State)
	}

	// No other string: the omitted lookup shows the join lookup_unanswered.
	dialled = nil
	answerFrom = ""
	lone := writeOpenJoin(t, strings.Repeat("ee", 32), "circlehub1lone", testmint.Hex32(t))
	out = settleOneJoin(newCashSettleCmd(), lone, mustCred(t))
	if len(dialled) != 1 || dialled[0] != "circlehub1lone" || out.Join.State != config.JoinShown || out.Join.Detail != "lookup_unanswered" {
		t.Errorf("lone join: dialled %v, %s/%s", dialled, out.Join.State, out.Join.Detail)
	}

	// The other string tried once already: never again; shown.
	dialled = nil
	tried := writeOpenJoin(t, hub, "circlehub1third", testmint.Hex32(t))
	_ = config.MarkJoinLookup(tried.ConnectionPubkey, false, true)
	tried, _, _ = config.FindJoin(tried.ConnectionPubkey)
	out = settleOneJoin(newCashSettleCmd(), tried, mustCred(t))
	if len(dialled) != 1 || dialled[0] != "circlehub1third" || out.Join.Detail != "lookup_unanswered" {
		t.Errorf("after the other string was tried: dialled %v, %s", dialled, out.Join.Detail)
	}

	// An answered lookup is never unanswered.
	dialled = nil
	answered := writeOpenJoin(t, strings.Repeat("dd", 32), "circlehub1answered", testmint.Hex32(t))
	_ = config.MarkJoinLookup(answered.ConnectionPubkey, true, false)
	answered, _, _ = config.FindJoin(answered.ConnectionPubkey)
	if out = settleOneJoin(newCashSettleCmd(), answered, mustCred(t)); out.Join.Detail == "lookup_unanswered" {
		t.Error("a join the Hub once answered was shown unanswered")
	}
}

func mustOnlyOther(t *testing.T, conn string) string {
	t.Helper()
	all, _ := config.Joins()
	for _, j := range all {
		if j.HubConnection == conn {
			return j.ConnectionPubkey
		}
	}
	t.Fatal("no join over " + conn)
	return ""
}

func mustCred(t *testing.T) nipcw.Credential {
	t.Helper()
	cred, err := joinCredential(newCashSettleCmd(), "pubkey:"+testMemberPriv)
	if err != nil {
		t.Fatal(err)
	}
	return cred
}
