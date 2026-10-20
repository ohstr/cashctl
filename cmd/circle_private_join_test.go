package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip47"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcw"
	nipcwclient "github.com/ohstr/nmilat/nipcw/client"
	relayclient "github.com/ohstr/nmilat/relay/client"

	"github.com/ohstr/cashctl/internal/output"
)

// runTestJoinWith runs join against the fake Hub with the given flags and
// returns what it printed.
func runTestJoinWith(t *testing.T, flags map[string]string) (stdout, stderr string, err error) {
	t.Helper()
	c := newTestJoinCmd()
	for k, v := range flags {
		if setErr := c.Flags().Set(k, v); setErr != nil {
			t.Fatal(setErr)
		}
	}
	hub := nip47.BuildPairingURI(strings.Repeat("ab", 32), []string{"wss://hub.example"}, strings.Repeat("cd", 32), nil)
	stdout, stderr = captureStreams(t, func() { err = runCircleJoin(c, []string{hub, "500"}) })
	return stdout, stderr, err
}

func answeringJoin(t *testing.T) *fakeJoin {
	f := &fakeJoin{t: t, dialed: true}
	setUpJoinTest(t, f)
	f.resp = &nipcw.CreateCircleWalletResponse{WalletPubkey: strings.Repeat("ef", 32), Relays: []string{"wss://w.example"}}
	joinCircleFn = func(ctx context.Context, uri string, p nipcw.CreateCircleWalletParams, opts nipcwclient.JoinOptions) (*nipcwclient.JoinResult, bool, error) {
		f.resp.PairingURI = nip47.BuildPairingURI(f.resp.WalletPubkey, f.resp.Relays, p.Connection.Secret(), nil)
		return f.call(ctx, uri, p, opts)
	}
	return f
}

// A join reports how it travelled: private when the Hub offers it, and a
// fallback both on stderr and as fallback_reason, never silently.
func TestJoin_ReportsTheTransport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		private     bool
		fallback    error
		wantPrivate bool
		wantReason  string
	}{
		{"no identity", false, nil, false, ""},
		{"private", true, nil, true, ""},
		{"fallback", false, errors.New("announcement not found"), false, "announcement not found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := answeringJoin(t)
			f.private, f.fallback = tc.private, tc.fallback
			stdout, _, err := runTestJoinWith(t, map[string]string{"json": "true"})
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if f.opts.RequirePrivate {
				t.Error("RequirePrivate set without --private=require")
			}
			var got struct {
				Private        *bool   `json:"private"`
				FallbackReason *string `json:"fallback_reason"`
			}
			if err := json.Unmarshal([]byte(stdout), &got); err != nil {
				t.Fatalf("stdout is not JSON: %v\n%s", err, stdout)
			}
			if got.Private == nil || *got.Private != tc.wantPrivate {
				t.Errorf("private = %v, want %v", got.Private, tc.wantPrivate)
			}
			if got.FallbackReason == nil || *got.FallbackReason != tc.wantReason {
				t.Errorf("fallback_reason = %v, want %q", got.FallbackReason, tc.wantReason)
			}

			// Text mode: one line on stderr for a fallback, none otherwise.
			f2 := answeringJoin(t)
			f2.private, f2.fallback = tc.private, tc.fallback
			_, stderr, err := runTestJoinWith(t, map[string]string{"yes": "true"})
			if err != nil {
				t.Fatalf("join (text): %v", err)
			}
			line := "joined without private transport: " + tc.wantReason
			if said := strings.Contains(stderr, "joined without private transport"); said != (tc.fallback != nil) {
				t.Errorf("stderr = %q, want the fallback notice only on a fallback", stderr)
			} else if tc.fallback != nil && !strings.Contains(stderr, line) {
				t.Errorf("stderr = %q, want %q", stderr, line)
			}
		})
	}
}

// --private=require reaches nmilat as RequirePrivate, and its refusal sent
// nothing, so the key is forgotten and the message says why.
func TestJoin_RequirePrivateRefusal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode output.ErrorCode
		wantText string
	}{
		{"not offered", fmt.Errorf("%w: the Hub does not offer it", nipcwclient.ErrPrivateJoinUnavailable), output.CodeNotFound, "does not offer private join"},
		{"announcement unverified", fmt.Errorf("%w: %w", nipcwclient.ErrPrivateJoinUnavailable, errors.New("announcement signature invalid")), output.CodeNetwork, "announcement signature invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeJoin{t: t, dialed: true, err: tc.err}
			setUpJoinTest(t, f)
			_, _, err := runTestJoinWith(t, map[string]string{"json": "true", "private": "require"})
			if err == nil {
				t.Fatal("join succeeded, want a refusal")
			}
			if !f.opts.RequirePrivate {
				t.Error("--private=require did not set RequirePrivate")
			}
			ce := output.AsCLIError(err)
			if ce.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ce.Code, tc.wantCode)
			}
			for _, want := range []string{tc.wantText, "nothing was sent", "--private=require"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not say %q", err, want)
				}
			}
			if p := pendingJoins(t); len(p) != 0 {
				t.Errorf("pending joins = %+v, want none: nothing was sent", p)
			}
		})
	}
}

func TestJoin_RejectsAnUnknownPrivateValue(t *testing.T) {
	f := &fakeJoin{t: t, dialed: true}
	setUpJoinTest(t, f)
	_, _, err := runTestJoinWith(t, map[string]string{"json": "true", "private": "always"})
	if got := output.AsCLIError(err); got == nil || got.Code != output.CodeUsage {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

// A join that failed over the standard transport after the private one was
// unavailable says so, keeps the refusal's own code (SF-6), and keeps the key
// only when the outcome is unknown.
func TestJoin_FailureAfterFallbackNamesTheTransport(t *testing.T) {
	// The shape nmilat rc.28's Join returns (nipcw/client/join.go).
	afterFallback := func(err error) error {
		return fmt.Errorf("%w (after private join was unavailable: %v)", err, errors.New("no announcement on wss://hub.example"))
	}
	for _, tc := range []struct {
		name     string
		err      error
		wantCode output.ErrorCode
		wantKept bool
	}{
		{"refused", afterFallback(&relayclient.WalletError{Code: nip47.ErrRestricted, Message: "not a member"}), output.CodeAuth, false},
		{"wallet exists", afterFallback(&relayclient.WalletError{Code: nipcw.ErrWalletExists, Message: "already holds one"}), output.CodeExists, false},
		{"no answer", afterFallback(context.DeadlineExceeded), output.CodeNetwork, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeJoin{t: t, dialed: true, err: tc.err}
			setUpJoinTest(t, f)
			_, _, err := runTestJoinWith(t, map[string]string{"json": "true"})
			if err == nil {
				t.Fatal("join succeeded, want a failure")
			}
			ce := output.AsCLIError(err)
			if ce.Code != tc.wantCode {
				t.Errorf("code = %q, want %q", ce.Code, tc.wantCode)
			}
			if !strings.Contains(ce.Hint, "shared Hub connection, not the private transport") ||
				!strings.Contains(ce.Hint, "no announcement on wss://hub.example") {
				t.Errorf("hint = %q, want it to name the standard transport and why", ce.Hint)
			}
			if kept := len(pendingJoins(t)) == 1; kept != tc.wantKept {
				t.Errorf("key kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// A private join the Hub did not answer may have created the wallet, so the
// key is kept, as for a standard join with no answer.
func TestJoin_UnansweredPrivateJoinKeepsTheKey(t *testing.T) {
	f := &fakeJoin{t: t, dialed: true, err: &nipcashclient.NotServedError{Method: nipcw.MethodCreateCircleWallet}}
	setUpJoinTest(t, f)
	_, _, err := runTestJoinWith(t, map[string]string{"json": "true"})
	if err == nil {
		t.Fatal("join succeeded, want no answer")
	}
	if ce := output.AsCLIError(err); ce.Hint != "" && strings.Contains(ce.Hint, "shared Hub connection") {
		t.Errorf("hint %q claims a fallback that did not happen", ce.Hint)
	}
	if p := pendingJoins(t); len(p) != 1 || !strings.Contains(err.Error(), p[0].ConnectionPubkey) {
		t.Errorf("pending joins = %+v, err = %v; want the key kept and named", p, err)
	}
}
