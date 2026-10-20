package cmd

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	relayclient "github.com/ohstr/nmilat/relay/client"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// fakeJoin stands in for the Hub. It records whether the connection key's
// secret was already saved when the request went out, then answers as told.
type fakeJoin struct {
	t           *testing.T
	resp        *nipcw.CreateCircleWalletResponse
	dialed      bool
	err         error
	sentKey     nipcw.ConnectionKey
	savedBefore bool
	opts        nipcwclient.JoinOptions
	private     bool
	fallback    error
}

func (f *fakeJoin) call(_ context.Context, _ string, p nipcw.CreateCircleWalletParams, opts nipcwclient.JoinOptions) (*nipcwclient.JoinResult, bool, error) {
	f.opts = opts
	f.sentKey = p.Connection
	pending, err := config.PendingJoins()
	if err != nil {
		f.t.Fatal(err)
	}
	for _, j := range pending {
		if j.ConnectionPubkey == p.Connection.Pubkey() && j.ConnectionSecret == p.Connection.Secret() {
			f.savedBefore = true
		}
	}
	if f.err != nil {
		return nil, f.dialed, f.err
	}
	return &nipcwclient.JoinResult{CreateCircleWalletResponse: f.resp, Private: f.private, FallbackReason: f.fallback}, f.dialed, nil
}

func setUpJoinTest(t *testing.T, f *fakeJoin) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	t.Setenv(asCredentialEnv, "pubkey:"+strings.Repeat("11", 32))
	prev := joinCircleFn
	joinCircleFn = f.call
	t.Cleanup(func() { joinCircleFn = prev })
}

func runTestJoin(t *testing.T) error {
	t.Helper()
	c := newTestJoinCmd()
	_ = c.Flags().Set("json", "true")
	hub := nip47.BuildPairingURI(strings.Repeat("ab", 32), []string{"wss://hub.example"}, strings.Repeat("cd", 32), nil)
	var err error
	captureStreams(t, func() { err = runCircleJoin(c, []string{hub, "500"}) })
	return err
}

func pendingJoins(t *testing.T) []config.PendingJoin {
	t.Helper()
	p, err := config.PendingJoins()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// The wallet is registered under the connection string built from the Hub's
// reply and the key cashctl generated, and the pending record goes with it.
func TestJoin_SavesTheKeyFirstAndRegistersTheWalletFromTheReply(t *testing.T) {
	f := &fakeJoin{t: t, dialed: true}
	setUpJoinTest(t, f)
	f.resp = &nipcw.CreateCircleWalletResponse{WalletPubkey: strings.Repeat("ef", 32), Relays: []string{"wss://w.example"}}
	// What nmilat builds from the reply and the member's own secret.
	joinCircleFn = func(ctx context.Context, uri string, p nipcw.CreateCircleWalletParams, opts nipcwclient.JoinOptions) (*nipcwclient.JoinResult, bool, error) {
		f.resp.PairingURI = nip47.BuildPairingURI(f.resp.WalletPubkey, f.resp.Relays, p.Connection.Secret(), nil)
		return f.call(ctx, uri, p, opts)
	}
	if err := runTestJoin(t); err != nil {
		t.Fatalf("join: %v", err)
	}
	if !f.savedBefore {
		t.Error("the connection secret was not saved before the request was sent")
	}
	if f.sentKey.IsZero() || f.sentKey.Pubkey() == "" {
		t.Fatal("no member-generated connection key was sent")
	}
	s, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Connections) != 1 || !strings.Contains(s.Connections[0].Value, "secret="+f.sentKey.Secret()) {
		t.Errorf("registered connections = %+v, want one built with the generated secret", s.Connections)
	}
	if p := pendingJoins(t); len(p) != 0 {
		t.Errorf("pending joins after success = %+v, want none", p)
	}
}

// A refusal or a request that never left created nothing, so the key is
// forgotten; no answer at all may have created the wallet, so it is kept and
// the error names the connection key the Hub knows it by.
func TestJoin_KeepsTheKeyOnlyWhenTheOutcomeIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dialed   bool
		err      error
		wantKept bool
		wantCode output.ErrorCode
	}{
		{"refused", true, &relayclient.WalletError{Code: "WALLET_EXISTS", Message: "already holds one"}, false, output.CodeExists},
		{"dial failed", false, errors.New("dial: refused"), false, output.CodeNetwork},
		{"no answer", true, context.DeadlineExceeded, true, output.CodeNetwork},
		// A Hub from before member-generated keys answers with no relays: the
		// wallet likely exists, and a retry would only meet it.
		{"unusable reply", true, errors.New("nipcw: wallet details carry no usable relay, so the wallet is unreachable"), true, output.CodeInternal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeJoin{t: t, dialed: tc.dialed, err: tc.err}
			setUpJoinTest(t, f)
			err := runTestJoin(t)
			if err == nil {
				t.Fatal("join succeeded, want a failure")
			}
			if got := output.AsCLIError(err).Code; got != tc.wantCode {
				t.Errorf("code = %q, want %q", got, tc.wantCode)
			}
			p := pendingJoins(t)
			if kept := len(p) == 1; kept != tc.wantKept {
				t.Fatalf("pending joins = %+v, want kept=%v", p, tc.wantKept)
			}
			if tc.wantKept {
				if p[0].ConnectionPubkey != f.sentKey.Pubkey() || p[0].ConnectionSecret != f.sentKey.Secret() {
					t.Errorf("kept %+v, want the key that was sent", p[0])
				}
				if !strings.Contains(err.Error(), f.sentKey.Pubkey()) {
					t.Errorf("error %q does not name the connection key", err)
				}
			}
		})
	}
}

// A join with no answer shows in wallet show by its connection key, in both
// modes, and never with its secret.
func TestWalletShow_ListsPendingJoinsWithoutTheSecret(t *testing.T) {
	f := &fakeJoin{t: t, dialed: true, err: context.DeadlineExceeded}
	setUpJoinTest(t, f)
	if err := runTestJoin(t); err == nil {
		t.Fatal("join succeeded, want no answer")
	}
	for _, jsonMode := range []bool{true, false} {
		cmd := newWalletShowCmd()
		cmd.Flags().Bool("json", jsonMode, "")
		cmd.SetArgs(nil)
		printed := withCapturedOutput(func() {
			if err := cmd.Execute(); err != nil {
				t.Errorf("wallet show (json=%v): %v", jsonMode, err)
			}
		})
		if !strings.Contains(printed, f.sentKey.Pubkey()) {
			t.Errorf("wallet show (json=%v) does not list the pending join:\n%s", jsonMode, printed)
		}
		if strings.Contains(printed, f.sentKey.Secret()) {
			t.Errorf("wallet show (json=%v) prints the connection secret", jsonMode)
		}
	}
}
