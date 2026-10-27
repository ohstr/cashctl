package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

// fakeCircleHub stands in for nipcw/client: it runs Join's checks in the same
// order (accepting, announcement, rotation warning, write-ahead) and then
// answers as told.
type fakeCircleHub struct {
	t          *testing.T
	info       *nip47.GetInfoResult
	dialErr    error
	noAnnounce bool
	// answer is the outcome after the write-ahead.
	answer func(pj nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error)
	lookup func(pj nipcwclient.PendingJoin) (nipcwclient.JoinAction, error)

	dials, joins, lookups int
	// writtenBeforeSend: the join row held the secret when the join went out.
	writtenBeforeSend bool
	deadline          int64
}

func (f *fakeCircleHub) Info(context.Context) (*nip47.GetInfoResult, error) { return f.info, nil }
func (f *fakeCircleHub) Verified() bool                                     { return true }
func (f *fakeCircleHub) Close()                                             {}

func (f *fakeCircleHub) Join(ctx context.Context, p nipcw.CreateCircleWalletParams, o nipcwclient.JoinOptions, j nipcwclient.JoinJournal) (*nipcwclient.JoinResult, error) {
	cw := f.info.CircleWallet
	if cw.Accepting == nil || !*cw.Accepting {
		return nil, nipcwclient.ErrNotAccepting
	}
	if f.noAnnounce {
		return nil, errors.Join(nipcwclient.ErrNoAnnouncement, errors.New("no 11190"))
	}
	if cw.RotationDelaySecs == nil {
		if o.WarnNoRotation == nil {
			return nil, nipcwclient.ErrRotationWarningRequired
		}
		if !o.WarnNoRotation(ctx) {
			return nil, nipcwclient.ErrJoinDeclined
		}
	}
	key, err := nipcw.NewConnectionKey()
	if err != nil {
		f.t.Fatal(err)
	}
	deadline := f.deadline
	if deadline == 0 {
		deadline = time.Now().Unix() + 300
	}
	pj := nipcwclient.PendingJoin{HubPubkey: strings.Repeat("ab", 32), Pubkey: testMemberPub(f.t),
		ConnectionPubkey: key.Pubkey(), ConnectionSecret: key.Secret(), Deadline: deadline}
	if err := j.WriteAhead(ctx, pj); err != nil {
		return nil, errors.Join(nipcwclient.ErrJoinNotSent, err)
	}
	if row, ok, _ := config.FindJoin(pj.ConnectionPubkey); ok && row.ConnectionSecret == pj.ConnectionSecret {
		f.writtenBeforeSend = true
	}
	f.joins++
	// What nmilat does right after it publishes the request: run the hook the
	// caller put on ctx (WithAfterPublish). A call placed without it never
	// reaches calls.after_send.
	if f := relayclient.AfterPublishFrom(ctx); f != nil {
		f()
	}
	return f.answer(pj)
}

func (f *fakeCircleHub) LookupJoin(_ context.Context, pj nipcwclient.PendingJoin, _ nipcw.Credential) (nipcwclient.JoinAction, error) {
	f.lookups++
	if f.lookup == nil {
		return nipcwclient.JoinAction{Kind: nipcwclient.JoinWait}, nil
	}
	return f.lookup(pj)
}

const testMemberPriv = "1111111111111111111111111111111111111111111111111111111111111111"

func testMemberPub(t *testing.T) string {
	pub, err := utils.GetPublicKey(testMemberPriv)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func goodCircleInfo() *nip47.GetInfoResult {
	dust := int64(1000)
	delay := int64(86400)
	return &nip47.GetInfoResult{Network: "mainnet", Chain: "flokicoin", CircleWallet: &nip47.CircleWalletInfo{
		Identity: strings.Repeat("cd", 32), Accepting: ptrTo(true), FeesPpm: 1000,
		FeeReserve: &nip47.FeeReserve{Ppm: 5000, MinMillis: 1000}, DustThresholdMillis: &dust,
		RotationDelaySecs: &delay, PerWalletMaxReceiveMillis: 2_000_000,
	}}
}

// walletResponse answers a join with wallet details for its key.
func walletResponse(pj nipcwclient.PendingJoin) *nipcw.CreateCircleWalletResponse {
	return &nipcw.CreateCircleWalletResponse{WalletPubkey: strings.Repeat("ef", 32), ConnectionPubkey: pj.ConnectionPubkey,
		Relays: []string{"wss://wallet.example"}, MaxAmountMillis: 500_000, MaxReceiveMillis: 2_000_000,
		FeesPpm: 1000, DustThresholdMillis: 1000, BudgetRenewal: "monthly"}
}

func setUpCircleHub(t *testing.T, f *fakeCircleHub) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	t.Setenv(asCredentialEnv, "pubkey:"+testMemberPriv)
	f.t = t
	if f.info == nil {
		f.info = goodCircleInfo()
	}
	if f.answer == nil {
		f.answer = func(pj nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error) {
			return &nipcwclient.JoinResult{CreateCircleWalletResponse: walletResponse(pj), Pending: pj}, nil
		}
	}
	prev := dialCircleHubFn
	dialCircleHubFn = func(context.Context, string) (circleHub, error) {
		f.dials++
		if f.dialErr != nil {
			return nil, f.dialErr
		}
		return f, nil
	}
	t.Cleanup(func() { dialCircleHubFn = prev })
}

// runJoinCLI runs join with --json and flags; stdout is its JSON.
func runJoinCLI(t *testing.T, hub string, flags map[string]string) (stdout, stderr string, err error) {
	t.Helper()
	c := newTestJoinCmd()
	_ = c.Flags().Set("json", "true")
	for k, v := range flags {
		if sErr := c.Flags().Set(k, v); sErr != nil {
			t.Fatal(sErr)
		}
	}
	stdout, stderr = captureStreams(t, func() { err = runCircleJoin(c, []string{hub, "500"}) })
	return stdout, stderr, err
}

func testCircleHub(t *testing.T) string {
	hubPriv, _ := testmint.Key(t)
	return testmint.CircleHub(t, hubPriv, "Ada's Circle")
}

// unverifiedCircleHub is a well-formed circlehub1 whose signature does not
// cover its text (the label was changed after signing).
func unverifiedCircleHub(t *testing.T) string {
	c, err := nipcw.DecodeCircleHubConnection(testCircleHub(t))
	if err != nil {
		t.Fatal(err)
	}
	c.Label = "Someone else's circle"
	s, err := nipcw.EncodeCircleHubConnection(c)
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := nipcw.DecodeCircleHubConnection(s); d.Verify() == nil {
		t.Fatal("fixture still verifies")
	}
	return s
}

func allJoins(t *testing.T) []config.Join {
	t.Helper()
	j, err := config.Joins()
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// T-36: the join is written ahead with its secret before it goes out; the
// wallet is stored as its fields (no URI anywhere), the join closes done and
// forgets the secret, and --json carries the new keys.
func TestJoin_WritesAheadAndStoresTheWalletAsFields(t *testing.T) {
	f := &fakeCircleHub{}
	setUpCircleHub(t, f)
	stdout, _, err := runJoinCLI(t, testCircleHub(t), nil)
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if !f.writtenBeforeSend {
		t.Error("the join went out before its secret was written")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("stdout %q: %v", stdout, err)
	}
	for _, k := range []string{"network", "chain", "test_network", "hub_verified", "terms", "warnings"} {
		if _, ok := doc[k]; !ok {
			t.Errorf("--json lacks %q: %s", k, stdout)
		}
	}
	for _, gone := range []string{"private", "fallback_reason"} {
		if _, ok := doc[gone]; ok {
			t.Errorf("--json still has %q", gone)
		}
	}
	resp := doc["response"].(map[string]any)
	for _, k := range []string{"max_receive_millis", "dust_threshold_millis", "rotation_delay_secs"} {
		if _, ok := resp[k]; !ok {
			t.Errorf("--json response lacks %q", k)
		}
	}
	if doc["hub_verified"] != true || doc["test_network"] != false {
		t.Errorf("hub_verified/test_network = %v/%v", doc["hub_verified"], doc["test_network"])
	}

	s, _ := config.Load()
	if len(s.Connections) != 1 || s.Connections[0].Kind != config.KindCircle {
		t.Fatalf("connections %+v", s.Connections)
	}
	v := s.Connections[0].Value
	if strings.Contains(v, "walletconnect") {
		t.Errorf("a URI was stored: %s", v)
	}
	w, ok, err := config.ParseCircleWallet(v)
	if !ok || err != nil || w.WalletPubkey != strings.Repeat("ef", 32) || w.Secret == "" || w.Chain != "flokicoin" {
		t.Errorf("stored wallet %+v ok=%v err=%v", w, ok, err)
	}
	joins := allJoins(t)
	if len(joins) != 1 || joins[0].State != config.JoinDone || joins[0].ConnectionSecret != "" || joins[0].WalletName != s.Connections[0].Name {
		t.Errorf("join row %+v, want done without its secret", joins)
	}
	if strings.Contains(stdout, w.Secret) {
		t.Error("the secret reached stdout")
	}
}

// Only a circlehub1 is joined from: the URI form and a cashhub1 are refused
// connection_invalid before anything is dialled.
func TestJoin_RefusesAnythingButACircleHubString(t *testing.T) {
	f := &fakeCircleHub{}
	setUpCircleHub(t, f)
	hubPriv, _ := testmint.Key(t)
	for name, hub := range map[string]string{
		"uri":     nip47.BuildPairingURI(strings.Repeat("ab", 32), []string{"wss://hub.example"}, strings.Repeat("cd", 32), nil),
		"cashhub": testmint.CashHub(t, hubPriv, ""),
	} {
		_, _, err := runJoinCLI(t, hub, nil)
		ce := output.AsCLIError(err)
		if ce == nil || output.ExitCode(err) != 3 || ce.Reason != reasonConnectionInvalid {
			t.Errorf("%s: err %v, want exit 3 connection_invalid", name, err)
		}
	}
	if f.dials != 0 {
		t.Errorf("dialled %d times", f.dials)
	}
}

// The online check failing is auth exit 7, connection_mismatch, nothing written.
func TestJoin_OnlineCheckMismatch(t *testing.T) {
	for _, cause := range []error{nipcash.ErrHubIdentityMismatch, nipcash.ErrChainMismatch} {
		f := &fakeCircleHub{dialErr: cause}
		setUpCircleHub(t, f)
		_, _, err := runJoinCLI(t, testCircleHub(t), nil)
		if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 7 || ce.Reason != reasonConnectionMismatch {
			t.Errorf("%v: err %v, want exit 7 connection_mismatch", cause, err)
		}
		if f.joins != 0 || len(allJoins(t)) != 0 {
			t.Errorf("%v: a join was written or sent", cause)
		}
	}
}

// accepting false → auth exit 7 not_accepting; no announcement → network exit 6
// join_unavailable; neither writes or sends anything.
func TestJoin_NotSentOutcomes(t *testing.T) {
	closed := goodCircleInfo()
	closed.CircleWallet.Accepting = ptrTo(false)
	for _, tc := range []struct {
		name   string
		f      *fakeCircleHub
		exit   int
		reason string
	}{
		{"not accepting", &fakeCircleHub{info: closed}, 7, reasonNotAccepting},
		{"no announcement", &fakeCircleHub{noAnnounce: true}, 6, reasonJoinUnavailable},
	} {
		setUpCircleHub(t, tc.f)
		_, _, err := runJoinCLI(t, testCircleHub(t), nil)
		if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != tc.exit || ce.Reason != tc.reason {
			t.Errorf("%s: err %v, want exit %d %s", tc.name, err, tc.exit, tc.reason)
		}
		if tc.f.joins != 0 || len(allJoins(t)) != 0 {
			t.Errorf("%s: a join was written or sent", tc.name)
		}
	}
}

// A refusal made nothing: the join is not_applied and its secret forgotten.
func TestJoin_RefusalForgetsTheKey(t *testing.T) {
	f := &fakeCircleHub{answer: func(nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error) {
		return nil, &relayclient.WalletError{Method: nipcw.MethodCreateCircleWallet, Code: "RESTRICTED", Message: "not a member"}
	}}
	setUpCircleHub(t, f)
	_, _, err := runJoinCLI(t, testCircleHub(t), nil)
	if ce := output.AsCLIError(err); ce == nil || ce.NWCCode != "RESTRICTED" {
		t.Fatalf("err %v, want RESTRICTED", err)
	}
	joins := allJoins(t)
	if len(joins) != 1 || joins[0].State != config.JoinNotApplied || joins[0].ConnectionSecret != "" {
		t.Errorf("join row %+v, want not_applied without its secret", joins)
	}
}

// CC-C2: REQUEST_REUSED is no failure: the join stays open, its secret kept.
func TestJoin_RequestReusedKeepsTheJoinOpen(t *testing.T) {
	f := &fakeCircleHub{answer: func(nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error) {
		return nil, &relayclient.WalletError{Method: nipcw.MethodCreateCircleWallet, Code: nipcw.ErrRequestReused}
	}}
	setUpCircleHub(t, f)
	_, _, err := runJoinCLI(t, testCircleHub(t), nil)
	if output.AsCLIError(err).Code != output.CodeIndeterminate {
		t.Fatalf("err %v, want indeterminate", err)
	}
	joins := allJoins(t)
	if len(joins) != 1 || joins[0].State == config.JoinNotApplied || joins[0].ConnectionSecret == "" {
		t.Errorf("join row %+v, want still open with its secret", joins)
	}
}

// A lost reply is looked up at once; pending is exit 8 with the call; `cash
// settle` past the deadline registers the wallet the lookup returns.
func TestJoin_LostReplyIsLookedUpThenSettled(t *testing.T) {
	f := &fakeCircleHub{deadline: time.Now().Unix() - 1, answer: func(pj nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error) {
		return nil, &nipcwclient.JoinIndeterminateError{Pending: pj, Cause: errors.New("no reply")}
	}}
	setUpCircleHub(t, f)
	_, _, err := runJoinCLI(t, testCircleHub(t), nil)
	ce := output.AsCLIError(err)
	if ce == nil || output.ExitCode(err) != 8 || ce.Call["method"] != nipcw.MethodCreateCircleWallet {
		t.Fatalf("err %v (call %v), want exit 8 with the join's call", err, ce)
	}
	if f.lookups != 1 {
		t.Errorf("lookups = %d, want 1 right away", f.lookups)
	}
	joins := allJoins(t)
	if len(joins) != 1 || joins[0].State != config.JoinPending || joins[0].ConnectionSecret == "" {
		t.Fatalf("join row %+v, want pending with its secret", joins)
	}
	view := withCapturedOutput(func() { _ = newWalletShowCmd().Execute() })
	if strings.Contains(view, joins[0].ConnectionSecret) {
		t.Error("wallet show printed the join's secret")
	}

	f.lookup = func(pj nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) {
		return nipcwclient.JoinAction{Kind: nipcwclient.JoinClose, Response: walletResponse(pj)}, nil
	}
	cmd := newCashSettleCmd()
	cmd.Flags().Bool("json", true, "")
	cmd.Flags().Bool("yes", true, "")
	stdout, _ := captureStreams(t, func() { err = runCashSettle(cmd, nil) })
	if err != nil {
		t.Fatalf("settle: %v", err)
	}
	var doc struct {
		Joins []joinSettleRow `json:"joins"`
	}
	if jErr := json.Unmarshal([]byte(stdout), &doc); jErr != nil || len(doc.Joins) != 1 || doc.Joins[0].State != config.JoinDone {
		t.Fatalf("settle joins %+v (%v): %s", doc.Joins, jErr, stdout)
	}
	s, _ := config.Load()
	if len(s.Connections) != 1 || s.Connections[0].Kind != config.KindCircle || s.Default != s.Connections[0].Name {
		t.Errorf("connections %+v default %q", s.Connections, s.Default)
	}
}

// unknown: the Hub never made it — not_applied, key forgotten, never rejoined
// automatically. expired: kept and shown.
func TestJoin_LookupUnknownAndExpired(t *testing.T) {
	for _, tc := range []struct {
		action nipcwclient.JoinAction
		state  string
	}{
		{nipcwclient.JoinAction{Kind: nipcwclient.JoinMayRejoin}, config.JoinNotApplied},
		{nipcwclient.JoinAction{Kind: nipcwclient.JoinShow, Reason: "join_expired"}, config.JoinShown},
	} {
		f := &fakeCircleHub{answer: func(pj nipcwclient.PendingJoin) (*nipcwclient.JoinResult, error) {
			return nil, &nipcwclient.JoinIndeterminateError{Pending: pj, Code: nipcw.ErrRequestReused}
		}, lookup: func(nipcwclient.PendingJoin) (nipcwclient.JoinAction, error) { return tc.action, nil }}
		setUpCircleHub(t, f)
		_, _, err := runJoinCLI(t, testCircleHub(t), nil)
		if err == nil {
			t.Fatalf("%v: no error", tc.action)
		}
		joins := allJoins(t)
		if len(joins) != 1 || joins[0].State != tc.state || f.joins != 1 {
			t.Errorf("%v: rows %+v joins sent %d", tc.action, joins, f.joins)
		}
	}
}

// T-109: an unverified string is used only once confirmed, after the online
// check; --json alone refuses (connection_unverified, nothing sent);
// --allow-unverified proceeds and is remembered for that string only.
func TestJoin_UnverifiedStringNeedsOneConfirmation(t *testing.T) {
	f := &fakeCircleHub{}
	setUpCircleHub(t, f)
	hub := unverifiedCircleHub(t)
	_, stderr, err := runJoinCLI(t, hub, nil)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 2 || ce.Reason != reasonConnectionUnverified {
		t.Fatalf("err %v, want usage exit 2 connection_unverified", err)
	}
	if f.dials != 1 || f.joins != 0 {
		t.Errorf("dials %d joins %d: the online check runs first, then nothing is sent", f.dials, f.joins)
	}
	_ = stderr
	if _, _, err := runJoinCLI(t, hub, map[string]string{"allow-unverified": "true"}); err != nil {
		t.Fatalf("with --allow-unverified: %v", err)
	}
	if ok, _ := config.UnverifiedConfirmed(hub); !ok {
		t.Error("the confirmation was not kept")
	}
	stdout, _, err := runJoinCLI(t, hub, nil)
	if err != nil {
		t.Fatalf("second join over the confirmed string: %v", err)
	}
	var doc map[string]any
	if jErr := json.Unmarshal([]byte(stdout), &doc); jErr != nil || doc["hub_verified"] != false {
		t.Errorf("a join over an unverified string is not marked: %s", stdout)
	}
	if ok, _ := config.UnverifiedConfirmed(unverifiedCircleHub(t)); ok {
		t.Error("another string counts as confirmed")
	}
}

// CC-C1: under --json a Hub without rotation is refused before the join, unless
// --backup-file or --accept-no-rotation says otherwise.
func TestJoin_NoRotationUnderJSONNeedsBackupOrFlag(t *testing.T) {
	info := goodCircleInfo()
	info.CircleWallet.RotationDelaySecs = nil
	f := &fakeCircleHub{info: info}
	setUpCircleHub(t, f)
	_, _, err := runJoinCLI(t, testCircleHub(t), nil)
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != reasonNoRotation {
		t.Fatalf("err %v, want refused with no_rotation", err)
	}
	if f.joins != 0 {
		t.Errorf("%d joins sent, want none", f.joins)
	}
	if _, _, err := runJoinCLI(t, testCircleHub(t), map[string]string{"accept-no-rotation": "true"}); err != nil {
		t.Fatalf("join with --accept-no-rotation: %v", err)
	}
}

// R-CW-09: a Hub without rotation gets the warning first, and --backup-file
// writes the wallet's fields (0600, no URI).
func TestJoin_NoRotationWarnsAndWritesTheBackup(t *testing.T) {
	info := goodCircleInfo()
	info.CircleWallet.RotationDelaySecs = nil
	f := &fakeCircleHub{info: info}
	setUpCircleHub(t, f)
	path := filepath.Join(t.TempDir(), "wallet.json")
	stdout, _, err := runJoinCLI(t, testCircleHub(t), map[string]string{"backup-file": path})
	if err != nil {
		t.Fatalf("join: %v", err)
	}
	if !strings.Contains(stdout, "can't be reached by anyone") {
		t.Errorf("--json warnings lack the rotation warning: %s", stdout)
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("backup %v mode %v", err, st)
	}
	b, _ := os.ReadFile(path)
	w, ok, err := config.ParseCircleWallet(string(b))
	if !ok || err != nil || w.HubPubkey == "" || w.HubIdentity == "" || strings.Contains(string(b), "walletconnect") {
		t.Errorf("backup %s", b)
	}
	if strings.Contains(stdout, w.Secret) {
		t.Error("the secret reached stdout")
	}
}

// --max-receive is required unless the Hub reports a default.
func TestJoin_MaxReceiveRequiredWithoutAHubDefault(t *testing.T) {
	info := goodCircleInfo()
	info.CircleWallet.PerWalletMaxReceiveMillis = 0
	f := &fakeCircleHub{info: info}
	setUpCircleHub(t, f)
	_, _, err := runJoinCLI(t, testCircleHub(t), nil)
	if output.ExitCode(err) != 2 || !strings.Contains(err.Error(), "--max-receive") {
		t.Fatalf("err %v, want usage naming --max-receive", err)
	}
	if _, _, err := runJoinCLI(t, testCircleHub(t), map[string]string{"max-receive": "10"}); err != nil {
		t.Fatalf("with --max-receive: %v", err)
	}
}

// A stored Circle Wallet is a wallet connection; a malformed one is refused.
func TestCircleWalletValue_IsAConnection(t *testing.T) {
	w := config.CircleWallet{WalletPubkey: strings.Repeat("ef", 32), Relays: []string{"wss://w.example"}, Secret: strings.Repeat("12", 32)}
	v, _ := w.Encode()
	if err := validateConnectionValue(v); err != nil {
		t.Errorf("stored wallet refused: %v", err)
	}
	w.Secret = "nope"
	bad, _ := w.Encode()
	if err := validateConnectionValue(bad); err == nil {
		t.Error("a malformed wallet was accepted")
	}
}
