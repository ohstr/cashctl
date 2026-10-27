package cmd

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcw"
	relayclient "github.com/ohstr/nmilat/relay/client"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
)

// fakeCircleWallet stands in for a member's own Circle Wallet connection.
type fakeCircleWallet struct {
	balance    int64
	balanceErr error
	deleteErr  error
	deletes    int
	// markedFirst: the deletion was written ahead when it went out.
	markedFirst bool
	name        string
	t           *testing.T
}

func (f *fakeCircleWallet) GetBalance(context.Context) (*nip47.GetBalanceResult, error) {
	if f.balanceErr != nil {
		return nil, f.balanceErr
	}
	return &nip47.GetBalanceResult{BalanceMloki: f.balance}, nil
}

func (f *fakeCircleWallet) DeleteWallet(context.Context) error {
	f.deletes++
	s, _ := config.Load()
	if c, ok := s.Find(f.name); ok {
		w, _, _ := config.ParseCircleWallet(c.Value)
		f.markedFirst = w.DeleteSentAt != 0
	}
	return f.deleteErr
}

func (f *fakeCircleWallet) Close() {}

func setUpCircleWallet(t *testing.T, f *fakeCircleWallet) {
	prev := dialCircleWalletFn
	dialCircleWalletFn = func(context.Context, string) (circleWalletClient, error) { return f, nil }
	t.Cleanup(func() { dialCircleWalletFn = prev })
}

func runDeleteCLI(t *testing.T, args []string, yes bool) (map[string]any, error) {
	t.Helper()
	c := newWalletDeleteCmd()
	c.Flags().Bool("json", true, "")
	c.Flags().Bool("yes", yes, "")
	c.Flags().StringP("connection", "c", "", "")
	var err error
	stdout, _ := captureStreams(t, func() { err = runWalletDelete(c, args) })
	var doc map[string]any
	if err == nil {
		_ = json.Unmarshal([]byte(stdout), &doc)
	}
	return doc, err
}

func heldWallet(t *testing.T, name string) (config.CircleWallet, bool) {
	s, _ := config.Load()
	c, ok := s.Find(name)
	if !ok {
		return config.CircleWallet{}, false
	}
	w, _, _ := config.ParseCircleWallet(c.Value)
	return w, true
}

// T-75: above dust → conflict, nothing sent; --json without --yes → usage,
// nothing sent; with --yes, written ahead, sent, forgotten here.
func TestWalletDelete_DustBoundaryConsentAndWriteAhead(t *testing.T) {
	name := joinRotatable(t, &fakeCircleHub{}, nil)
	fw := &fakeCircleWallet{balance: 1001, name: name, t: t}
	setUpCircleWallet(t, fw)

	_, err := runDeleteCLI(t, []string{name}, true)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 5 || ce.Reason != reasonAboveDust || fw.deletes != 0 {
		t.Errorf("above dust: %v, deletes %d", err, fw.deletes)
	}
	fw.balance = 1000
	_, err = runDeleteCLI(t, []string{name}, false)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 2 || ce.Reason != reasonDustNotAccepted || fw.deletes != 0 {
		t.Errorf("--json without --yes: %v, deletes %d", err, fw.deletes)
	}
	doc, err := runDeleteCLI(t, []string{name}, true)
	if err != nil || fw.deletes != 1 || !fw.markedFirst {
		t.Fatalf("delete: %v, deletes %d, written ahead %v", err, fw.deletes, fw.markedFirst)
	}
	if doc["deleted"] != name || doc["dust_left_millis"] != float64(1000) {
		t.Errorf("--json %v", doc)
	}
	if _, ok := heldWallet(t, name); ok {
		t.Error("the deleted wallet is still held")
	}
}

// A lost reply keeps the wallet, marked; a refusal clears the mark.
func TestWalletDelete_LostReplyAndRefusal(t *testing.T) {
	name := joinRotatable(t, &fakeCircleHub{}, nil)
	fw := &fakeCircleWallet{balance: 0, name: name, t: t, deleteErr: context.DeadlineExceeded}
	setUpCircleWallet(t, fw)
	_, err := runDeleteCLI(t, []string{name}, true)
	if ce := output.AsCLIError(err); ce == nil || output.ExitCode(err) != 8 || ce.Reason != reasonDeleteUnknown || ce.Call["method"] != nipcw.MethodDeleteCircleWallet {
		t.Errorf("lost reply: %v", err)
	}
	if w, ok := heldWallet(t, name); !ok || w.DeleteSentAt == 0 {
		t.Errorf("after a lost reply: held %v, %+v", ok, w)
	}
	// The next attempt gets no answer at all: still unknown, still held.
	fw.balanceErr = context.DeadlineExceeded
	if _, err := runDeleteCLI(t, []string{name}, true); output.ExitCode(err) != 8 {
		t.Errorf("silent Hub after a sent delete: %v, want exit 8", err)
	}
	// The wallet answers and refuses the deletion: the mark goes.
	fw.balanceErr, fw.deleteErr = nil, &relayclient.WalletError{Method: nipcw.MethodDeleteCircleWallet, Code: nipcw.ErrRestricted, Message: "a payment is in flight"}
	if _, err := runDeleteCLI(t, []string{name}, true); output.ExitCode(err) != 7 {
		t.Errorf("RESTRICTED: %v, want exit 7", err)
	}
	if w, _ := heldWallet(t, name); w.DeleteSentAt != 0 {
		t.Error("a refused deletion stays marked")
	}
}

// Only a circle wallet is deleted on its Hub; connect rm refuses one
// without --force.
func TestWalletDelete_CircleOnlyAndConnectRmNeedsForce(t *testing.T) {
	name := joinRotatable(t, &fakeCircleHub{}, nil)
	s, _ := config.Load()
	if err := s.Add("outside", nip47.BuildPairingURI(strings.Repeat("ab", 32), []string{"wss://r.example"}, strings.Repeat("cd", 32), nil)); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeleteCLI(t, []string{"outside"}, true); output.ExitCode(err) != 3 {
		t.Errorf("delete of an outside wallet: %v, want exit 3", err)
	}
	rm := newConnectRmCmd()
	rm.Flags().Bool("json", true, "")
	var err error
	captureStreams(t, func() { err = rm.RunE(rm, []string{name}) })
	if output.ExitCode(err) != 2 {
		t.Errorf("connect rm of a circle wallet: %v, want usage", err)
	}
	_ = rm.Flags().Set("force", "true")
	captureStreams(t, func() { err = rm.RunE(rm, []string{name}) })
	if err != nil {
		t.Errorf("connect rm --force: %v", err)
	}
}
