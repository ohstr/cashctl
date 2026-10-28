package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/output"
)

const testOtherPriv = "2222222222222222222222222222222222222222222222222222222222222222"

// joinRotatable joins through the fake Hub; delay is the wallet's rotation
// delay in its details (nil: a wallet that cannot be rotated). It returns
// the wallet's name.
func joinRotatable(t *testing.T, f *fakeCircleHub, delay *uint64) string {
	t.Helper()
	f.answer = func(pj nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error) {
		r := walletResponse(pj)
		r.RotationDelaySecs = delay
		return &nipcwclient.JoinResult{CreateCircleWalletResponse: r, Pending: pj}, nil
	}
	setUpCircleHub(t, f)
	if _, _, err := runJoinCLI(t, testCircleHub(t), nil); err != nil {
		t.Fatalf("join: %v", err)
	}
	s, _ := config.Load()
	if len(s.Connections) != 1 {
		t.Fatalf("connections %+v", s.Connections)
	}
	return s.Connections[0].Name
}

func newTestRequestCmd() *cobra.Command {
	c := newCircleRequestCmd()
	c.Flags().Bool("json", false, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().StringP("connection", "c", "", "")
	return c
}

// runRequestCLI runs `circle request` with --json; stdout is its JSON.
func runRequestCLI(t *testing.T, args []string, flags map[string]string) (doc map[string]any, stdout string, err error) {
	t.Helper()
	c := newTestRequestCmd()
	_ = c.Flags().Set("json", "true")
	for k, v := range flags {
		if sErr := c.Flags().Set(k, v); sErr != nil {
			t.Fatal(sErr)
		}
	}
	stdout, _ = captureStreams(t, func() { err = runCircleRequest(c, args) })
	if err == nil {
		if jErr := json.Unmarshal([]byte(stdout), &doc); jErr != nil {
			t.Fatalf("stdout %q: %v", stdout, jErr)
		}
	}
	return doc, stdout, err
}

func eventOf(t *testing.T, doc map[string]any) *nip01.Event {
	t.Helper()
	b, _ := json.Marshal(doc["event"])
	var ev nip01.Event
	if err := json.Unmarshal(b, &ev); err != nil {
		t.Fatal(err)
	}
	return &ev
}

func rotations(t *testing.T) []config.Join {
	t.Helper()
	var out []config.Join
	for _, j := range allJoins(t) {
		if j.IsRotation() {
			out = append(out, j)
		}
	}
	return out
}

var hubPub = strings.Repeat("ab", 32)
var walletPub = strings.Repeat("ef", 32)

// T-76: a rotate's new key is written ahead before the request is shown; the
// request is the member's, for this Hub and wallet, naming that key.
func TestCircleRequest_RotateWritesTheKeyAheadAndSignsForTheMember(t *testing.T) {
	delay := uint64(3600)
	name := joinRotatable(t, &fakeCircleHub{}, &delay)
	var writtenFirst bool
	restore := crashpoint.SetHook(func(point string) {
		if point == "rotation.after_writeahead" {
			rs := rotations(t)
			writtenFirst = len(rs) == 1 && rs[0].ConnectionSecret != ""
		}
	})
	defer restore()
	doc, stdout, err := runRequestCLI(t, []string{"rotate", name}, nil)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if !writtenFirst {
		t.Error("the new key was not written ahead before the request was shown")
	}
	ev := eventOf(t, doc)
	mr, err := nipcw.VerifyMemberRequest(ev, hubPub, time.Now(), nil)
	if err != nil {
		t.Fatalf("the request does not verify for the Hub: %v", err)
	}
	if mr.Action != nipcw.ActionRotate || mr.Wallet != walletPub || mr.Pubkey != testMemberPub(t) || mr.Connection != doc["new_connection_pubkey"] {
		t.Errorf("request %+v, doc %v", mr, doc)
	}
	if doc["submit_within"] != float64(ev.CreatedAt+memberRequestValidity) {
		t.Errorf("submit_within = %v", doc["submit_within"])
	}
	rs := rotations(t)
	if len(rs) != 1 {
		t.Fatalf("rotation rows %+v", rs)
	}
	r := rs[0]
	if r.ConnectionPubkey != mr.Connection || r.WalletPubkey != walletPub || r.WalletName != name || r.State != config.JoinWritten ||
		r.Deadline != int64(ev.CreatedAt)+memberRequestValidity || r.MemberPubkey != testMemberPub(t) || r.HubConnection == "" {
		t.Errorf("rotation row %+v", r)
	}
	if strings.Contains(stdout, r.ConnectionSecret) {
		t.Error("the new secret reached stdout")
	}

	// Asked again while it is open: the same request, not a second key.
	again, _, err := runRequestCLI(t, []string{"rotate", name}, nil)
	if err != nil {
		t.Fatalf("rotate again: %v", err)
	}
	if eventOf(t, again).ID != ev.ID || again["repeated"] != true || len(rotations(t)) != 1 {
		t.Errorf("second rotate: %v, rows %d", again, len(rotations(t)))
	}
}

// Freeze and unfreeze carry no connection tag and write nothing.
func TestCircleRequest_FreezeAndUnfreezeSignOnly(t *testing.T) {
	delay := uint64(3600)
	name := joinRotatable(t, &fakeCircleHub{}, &delay)
	for _, action := range []nipcw.MemberAction{nipcw.ActionFreeze, nipcw.ActionUnfreeze} {
		doc, _, err := runRequestCLI(t, []string{string(action), walletPub}, nil)
		if err != nil {
			t.Fatalf("%s: %v", action, err)
		}
		mr, err := nipcw.VerifyMemberRequest(eventOf(t, doc), hubPub, time.Now(), nil)
		if err != nil || mr.Action != action || mr.Connection != "" || mr.Wallet != walletPub {
			t.Errorf("%s: %+v %v", action, mr, err)
		}
		if _, ok := doc["new_connection_pubkey"]; ok {
			t.Errorf("%s names a new key", action)
		}
	}
	if len(rotations(t)) != 0 {
		t.Error("a freeze wrote a rotation row")
	}
	_ = name
}

// T-132: a wallet whose details carry no rotation delay is refused locally:
// exit 2 rotation_not_served, nothing signed, written or printed.
func TestCircleRequest_RotationNotServed(t *testing.T) {
	name := joinRotatable(t, &fakeCircleHub{}, nil)
	for _, action := range []string{"rotate", "freeze", "unfreeze"} {
		_, stdout, err := runRequestCLI(t, []string{action, name}, nil)
		ce := output.AsCLIError(err)
		if ce == nil || output.ExitCode(err) != 2 || ce.Reason != reasonRotationNotServed {
			t.Errorf("%s: err %v, want exit 2 rotation_not_served", action, err)
		}
		if stdout != "" {
			t.Errorf("%s printed %q", action, stdout)
		}
	}
	if len(rotations(t)) != 0 {
		t.Error("a rotation row was written")
	}
}

// Only the key that joined counts: another one is refused before signing.
func TestCircleRequest_OtherKeyRefused(t *testing.T) {
	delay := uint64(3600)
	name := joinRotatable(t, &fakeCircleHub{}, &delay)
	t.Setenv(asCredentialEnv, "pubkey:"+testOtherPriv)
	_, stdout, err := runRequestCLI(t, []string{"freeze", name}, nil)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 7 || ce.Reason != reasonMemberMismatch || stdout != "" {
		t.Errorf("err %v stdout %q, want exit 7 member_mismatch", err, stdout)
	}
}

// A lost device: a wallet not held here is named by --wallet and --hub.
func TestCircleRequest_WalletNotHeldHere(t *testing.T) {
	setUpCircleHub(t, &fakeCircleHub{})
	hub := testCircleHub(t)
	c, err := nipcw.DecodeCircleHubConnection(hub)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := runRequestCLI(t, []string{"freeze"}, map[string]string{"wallet": walletPub}); output.ExitCode(err) != 4 {
		t.Errorf("without --hub: %v, want not_found", err)
	}
	doc, _, err := runRequestCLI(t, []string{"freeze"}, map[string]string{"wallet": walletPub, "hub": hub})
	if err != nil {
		t.Fatal(err)
	}
	if mr, err := nipcw.VerifyMemberRequest(eventOf(t, doc), c.Pubkey, time.Now(), nil); err != nil || mr.Wallet != walletPub {
		t.Errorf("freeze for a wallet not held: %+v %v", mr, err)
	}
	doc, _, err = runRequestCLI(t, []string{"rotate"}, map[string]string{"wallet": walletPub, "hub": hub})
	if err != nil {
		t.Fatal(err)
	}
	if rs := rotations(t); len(rs) != 1 || rs[0].WalletName != "" || rs[0].HubConnection != hub || rs[0].ConnectionPubkey != doc["new_connection_pubkey"] {
		t.Errorf("rotation rows %+v", rs)
	}
}

func newTestSettleCmd() *cobra.Command {
	c := &cobra.Command{}
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", false, "")
	c.Flags().String("as", "", "")
	return c
}

// T-77: `cash settle` follows a rotation with the join lookup naming the new
// key: pending waits, done installs the new key in place of the old one.
func TestSettle_RotationPendingThenDoneReplacesTheKey(t *testing.T) {
	delay := uint64(3600)
	f := &fakeCircleHub{}
	name := joinRotatable(t, f, &delay)
	doc, _, err := runRequestCLI(t, []string{"rotate", name}, nil)
	if err != nil {
		t.Fatal(err)
	}
	newKey := doc["new_connection_pubkey"].(string)
	completes := time.Now().Unix() + 3600
	var asked nipcwclient.PendingJoin
	f.lookup = func(pj nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) {
		asked = pj
		return nipcwclient.JoinAction{Kind: nipcwclient.JoinWait, Until: completes}, nil
	}
	device, _ := deviceIDFn()
	rows, open := settleJoins(newTestSettleCmd(), device)
	if !open || asked.ConnectionPubkey != newKey || asked.Pubkey != testMemberPub(t) {
		t.Fatalf("pending: open %v, asked %+v, rows %+v", open, asked, rows)
	}
	if r := rotations(t)[0]; r.State != config.JoinPending || r.RetryAfter != completes {
		t.Errorf("after pending: %+v", r)
	}
	// Not due again before completes_at.
	before := f.lookups
	settleJoins(newTestSettleCmd(), device)
	if f.lookups != before {
		t.Error("asked again before completes_at")
	}

	_ = config.SetJoinState(newKey, config.JoinPending, "pending", time.Now().Unix()-1)
	f.lookup = func(pj nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) {
		r := walletResponse(pj)
		r.RotationDelaySecs = &delay
		return nipcwclient.JoinAction{Kind: nipcwclient.JoinClose, Response: r}, nil
	}
	rows, open = settleJoins(newTestSettleCmd(), device)
	if open {
		t.Errorf("still open after done: %+v", rows)
	}
	r := rotations(t)[0]
	if r.State != config.JoinDone || r.ConnectionSecret != "" || r.WalletName != name {
		t.Errorf("rotation row after done: %+v", r)
	}
	s, _ := config.Load()
	if len(s.Connections) != 1 {
		t.Fatalf("connections %+v", s.Connections)
	}
	w, _, _ := config.ParseCircleWallet(s.Connections[0].Value)
	key, _ := nipcw.ConnectionKeyFromSecret(w.Secret)
	if key.Pubkey() != newKey || w.WalletPubkey != walletPub {
		t.Errorf("the wallet's key is %s, want the rotated %s", key.Pubkey(), newKey)
	}
}

// A rotation the Hub never accepted is forgotten; the wallet keeps its key.
func TestSettle_RotationNeverAcceptedIsForgotten(t *testing.T) {
	delay := uint64(3600)
	f := &fakeCircleHub{}
	name := joinRotatable(t, f, &delay)
	s, _ := config.Load()
	before := s.Connections[0].Value
	if _, _, err := runRequestCLI(t, []string{"rotate", name}, nil); err != nil {
		t.Fatal(err)
	}
	f.lookup = func(nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) {
		return nipcwclient.JoinAction{Kind: nipcwclient.JoinMayRejoin}, nil
	}
	device, _ := deviceIDFn()
	rows, open := settleJoins(newTestSettleCmd(), device)
	if open {
		t.Errorf("open after unknown: %+v", rows)
	}
	if r := rotations(t)[0]; r.State != config.JoinNotApplied || r.ConnectionSecret != "" {
		t.Errorf("row %+v", r)
	}
	s, _ = config.Load()
	if s.Connections[0].Value != before {
		t.Error("the wallet changed")
	}
}

// T-78: while this device's rotate request is open, using the wallet asks
// first; --json alone refuses, --yes cancels it deliberately.
func TestRotationGuard_OldKeyUseAsksFirst(t *testing.T) {
	delay := uint64(3600)
	name := joinRotatable(t, &fakeCircleHub{}, &delay)
	s, _ := config.Load()
	value := s.Connections[0].Value
	if _, open := openRotationFor(value); open {
		t.Fatal("a rotation is open before any was requested")
	}
	if _, _, err := runRequestCLI(t, []string{"rotate", name}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := DialGeneric(context.Background(), value); !errors.Is(err, errRotationOpen) {
		t.Errorf("DialGeneric: %v, want errRotationOpen", err)
	}
	c := newTestRequestCmd()
	_ = c.Flags().Set("json", "true")
	if _, err := allowRotationUse(c, context.Background(), value); output.AsCLIError(err) == nil || output.AsCLIError(err).Reason != reasonRotationOpen {
		t.Errorf("--json: %v, want rotation_open", err)
	}
	_ = c.Flags().Set("yes", "true")
	ctx, err := allowRotationUse(c, context.Background(), value)
	if err != nil || !rotationUseAllowed(ctx) {
		t.Errorf("--yes: %v", err)
	}
}
