package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/testmint"
)

func TestAsProvenanceRefusal_SaysNmilatsRefusalAsCashctlsOwn(t *testing.T) {
	unverified := asProvenanceRefusal(fmt.Errorf("connect: %w", nipcashclient.ErrBillUnverified))
	if !errors.Is(unverified, errProvenanceFailed) || !errors.Is(unverified, nipcashclient.ErrBillUnverified) {
		t.Errorf("unverified bill = %v, want errProvenanceFailed wrapping ErrBillUnverified", unverified)
	}
	if unverified.Error() != errProvenanceFailed.Error() {
		t.Errorf("message = %q, want cashctl's own", unverified.Error())
	}
	for _, zapErr := range []error{nipcashclient.ErrZapContextRequired, nipcash.ErrZapContextRequired} {
		if got := asProvenanceRefusal(zapErr); !errors.Is(got, errNeedsZapEvent) || !isProvenanceRefusal(got) {
			t.Errorf("%v = %v, want errNeedsZapEvent", zapErr, got)
		}
	}

	// Anything else passes through untouched.
	for _, other := range []error{nil, nipcashclient.ErrNotSent, context.DeadlineExceeded, nipcashclient.ErrHubUnusable} {
		if got := asProvenanceRefusal(other); got != other { //nolint:errorlint // identity is the point
			t.Errorf("%v became %v", other, got)
		}
		if isProvenanceRefusal(other) {
			t.Errorf("%v reads as a provenance refusal", other)
		}
	}
}

func TestProvenanceRefusal_IsInvalidInputForReadsAndSpends(t *testing.T) {
	cmd := &cobra.Command{}
	refused := asProvenanceRefusal(nipcashclient.ErrBillUnverified)
	if err := classifyCashTokenNWCErr(cmd, refused); output.ExitCode(err) != 3 {
		t.Errorf("read: exit %d (%v), want 3", output.ExitCode(err), err)
	}
	if err := classifyCashSpendErr(cmd, refused, "transfer", "tok-a"); output.ExitCode(err) != 3 {
		t.Errorf("spend: exit %d (%v), want 3 (refused before anything was sent)", output.ExitCode(err), err)
	}
	// nmilat's sentinel on its own (a bill method refused when the session
	// opens) is the same refusal.
	if err := classifyCashTokenNWCErr(cmd, fmt.Errorf("status: %w", nipcashclient.ErrBillUnverified)); output.ExitCode(err) != 3 {
		t.Errorf("bare ErrBillUnverified: exit %d, want 3", output.ExitCode(err))
	}
}

// zapBill is a zap bill on relay and its zap context.
func zapBill(t *testing.T) (string, nipcash.ZapContext) {
	t.Helper()
	minterPriv, _ := testmint.Key(t)
	senderPriv, sender := testmint.Key(t)
	recipientPriv, recipient := testmint.Key(t)
	note, err := nip01.NewSignedEvent(1, "a note", recipientPriv)
	if err != nil {
		t.Fatal(err)
	}
	ev, bill := testmint.CashZap(t, minterPriv, senderPriv, note, 21000)
	issued := tagValue(ev, "issued_at")
	var at uint64
	if _, err := fmt.Sscan(issued, &at); err != nil {
		t.Fatal(err)
	}
	return bill, nipcash.ZapContext{Recipient: recipient, Sender: sender, ZapTarget: note.ID, IssuedAt: at}
}

func TestDialZap_ABillWhoseSignatureDoesNotRecoverIsRefusedAsCashctlSaysIt(t *testing.T) {
	bill, zc := zapBill(t)
	tok, err := nipcash.Decode(bill)
	if err != nil {
		t.Fatal(err)
	}
	for name, sig := range map[string][]byte{
		"zero signature":           make([]byte, 65),
		"recovery id out of range": append([]byte{0xff}, tok.MintSignature[1:]...),
	} {
		bad := tok
		bad.MintSignature = sig
		s, err := nipcash.Encode(bad)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		c, err := dialZapFn(context.Background(), s, zc)
		if c != nil || !errors.Is(err, errProvenanceFailed) || !errors.Is(err, nipcashclient.ErrBillUnverified) {
			t.Errorf("%s: client %v, err %v; want errProvenanceFailed", name, c, err)
		}
	}
	// The bill as minted, with its zap: nothing is dialled until a method runs.
	c, err := dialZapFn(context.Background(), bill, zc)
	if err != nil || c == nil {
		t.Fatalf("the bill as minted was refused: %v", err)
	}
	c.Close()
}

func TestDialBill_AVerifiedBillDialsNothingUntilAMethodRuns(t *testing.T) {
	relay, dialled := countingRelay(t)
	bill := testmint.Bill(t, nipcash.Token{RelayURLs: []string{relay}}, "")
	c, err := dialBill(context.Background(), bill)
	if err != nil {
		t.Fatalf("a bill as minted was refused: %v", err)
	}
	c.Close()
	if n := dialled.Load(); n != 0 {
		t.Errorf("connecting dialled the bill's relay %d time(s)", n)
	}

	// A copy whose signature does not recover is refused before its relay is
	// dialled.
	tok, err := nipcash.Decode(bill)
	if err != nil {
		t.Fatal(err)
	}
	tok.MintSignature = make([]byte, 65)
	broken, err := nipcash.Encode(tok)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialBill(context.Background(), broken); !isProvenanceRefusal(err) {
		t.Errorf("zero signature: %v, want a provenance refusal", err)
	}
	if n := dialled.Load(); n != 0 {
		t.Errorf("the broken copy's relay was dialled %d time(s)", n)
	}
}

func TestHubUnusable_IsNothingSentNeverIndeterminate(t *testing.T) {
	cmd := &cobra.Command{}
	rejected := fmt.Errorf("%w: newest announcement rejected: bad limits", nipcashclient.ErrHubUnusable)
	check := func(t *testing.T, err error) {
		t.Helper()
		ce := output.AsCLIError(err)
		if ce == nil || output.ExitCode(err) != 1 || ce.Reason != nipcashclient.ReasonHubUnusable {
			t.Fatalf("exit %d (%v), want 1 with reason hub_unusable", output.ExitCode(err), err)
		}
		if !strings.Contains(err.Error(), "sent it nothing") {
			t.Errorf("message does not say nothing was sent: %v", err)
		}
	}
	t.Run("a spend", func(t *testing.T) { check(t, classifyCashSpendErr(cmd, rejected, "transfer", "tok-a")) })
	t.Run("a read", func(t *testing.T) { check(t, classifyCashTokenNWCErr(cmd, rejected)) })
	t.Run("a spend stopped mid-batch", func(t *testing.T) {
		check(t, classifyCashSpendErr(cmd, fmt.Errorf("%w: %w", nipcashclient.ErrNotSent, rejected), "redeem", "tok-a"))
	})

	// The neighbours keep their own classes.
	if err := classifyCashSpendErr(cmd, context.DeadlineExceeded, "transfer", "tok-a"); output.ExitCode(err) != 8 {
		t.Errorf("a spend timeout: exit %d, want 8", output.ExitCode(err))
	}
	if err := classifyCashSpendErr(cmd, nipcashclient.ErrNotSent, "transfer", "tok-a"); output.AsCLIError(err).Reason != "" || output.ExitCode(err) != 1 {
		t.Errorf("not sent: %v, want exit 1 without a reason", err)
	}
	if err := classifyCashTokenNWCErr(cmd, context.DeadlineExceeded); output.ExitCode(err) != 6 {
		t.Errorf("a read timeout: exit %d, want 6", output.ExitCode(err))
	}
}

func TestPlaceErr_HubUnusableClosesTheWrittenCallNotApplied(t *testing.T) {
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	l, err := ledger.Load()
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	place := func(t *testing.T, cause error) (*ledger.Call, error) {
		t.Helper()
		j := &ledgerJournal{l: l}
		id, err := nipcash.NewRequestID()
		if err != nil {
			t.Fatal(err)
		}
		_, hub := testmint.Key(t)
		pc := nipcash.PendingCall{RequestID: id, Method: "cash_transfer", Deadline: time.Now().Unix() + 120, HubIdentity: hub, MaxRequestAge: 60, Chain: "flokicoin"}
		if err := j.WriteAhead(context.Background(), []nipcash.PendingCall{pc}); err != nil {
			t.Fatal(err)
		}
		c := j.call()
		return c, placeErr(cmd, l, j, cause, "transfer", "tok-a")
	}

	c, err := place(t, fmt.Errorf("%w: %w: newest announcement rejected", nipcashclient.ErrNotSent, nipcashclient.ErrHubUnusable))
	if ce := output.AsCLIError(err); ce == nil || ce.Reason != nipcashclient.ReasonHubUnusable || output.ExitCode(err) != 1 {
		t.Errorf("exit %d (%v), want 1 with reason hub_unusable", output.ExitCode(err), err)
	}
	if got, _ := l.FindCall(c.CallID); got.State != ledger.CallNotApplied {
		t.Errorf("call state %q, want %q: nothing was sent", got.State, ledger.CallNotApplied)
	}

	// No call written: the same refusal.
	if err := placeErr(cmd, l, &ledgerJournal{l: l}, nipcashclient.ErrHubUnusable, "transfer", "tok-a"); output.AsCLIError(err).Reason != nipcashclient.ReasonHubUnusable {
		t.Errorf("unwritten: %v, want reason hub_unusable", err)
	}

	// A written call with no answer stays open to be looked up.
	c, err = place(t, context.DeadlineExceeded)
	if output.ExitCode(err) != 8 {
		t.Errorf("no answer: exit %d, want 8", output.ExitCode(err))
	}
	if got, _ := l.FindCall(c.CallID); got.State != ledger.CallLookup {
		t.Errorf("call state %q, want %q", got.State, ledger.CallLookup)
	}
}
