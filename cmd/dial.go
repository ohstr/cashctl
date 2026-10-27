package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"

	"github.com/ohstr/cashctl/internal/ledger"
)

// Every bill cashctl contacts goes through dialBill: a bill's relays and secret
// are only trusted once its mint signature proves its minter wrote them
// (NIP-CASH §Mint Provenance). A copy whose provenance fails is refused before
// any relay is contacted, not even to fetch the Hub's announcement — a tampered
// copy names relays its attacker reads. dial_lint_test.go fails on any other
// nipcashclient.Connect in this package.

var (
	// errProvenanceFailed is a scheme-1 bill whose mint signature does not
	// recover over this copy's own relays and fields.
	errProvenanceFailed = errors.New("this copy's relays or secret differ from what its minter signed — ask the sender for the original")
	// errNeedsZapEvent is a zap bill (scheme 2) without the kind-9522 event its
	// signature covers; cashctl cannot check it on its own.
	errNeedsZapEvent = errors.New("this bill came from a Cash Zap and can only be checked with its zap event (kind 9522), which cashctl does not have")
	// errNoProvenance is a bill without a valid signature scheme: no provenance
	// means no route to trust (NIP-CASH §Mint Provenance).
	errNoProvenance = errors.New("this bill carries no valid mint signature, so cashctl cannot tell who issued it or which relays are really its Hub's — refusing to use it")
)

// checkBillProvenance is dialBill's decision, without the network: nil when tok
// may be dialled.
func checkBillProvenance(tok nipcash.Token) error {
	switch tok.Scheme {
	case nipcash.SchemeMintV1:
		if _, ok := nipcash.VerifyProvenance(tok); !ok {
			return errProvenanceFailed
		}
		return nil
	case nipcash.SchemeZapV1:
		return errNeedsZapEvent
	default:
		// SchemeNone, SchemeMalformed, or any value nmilat adds later.
		return errNoProvenance
	}
}

// dialBill connects to a bill's Hub, after its provenance check passed. token
// is the bill alone, never "<token>#<secret>". The minter is compared with the
// announcement's author by nmilat when the session opens.
func dialBill(ctx context.Context, token string) (*nipcashclient.Client, error) {
	tok, err := nipcash.Decode(token)
	if err != nil {
		return nil, err
	}
	if tok.Scheme == nipcash.SchemeZapV1 {
		// A zap bill is checked against the Cash Zap it was received with.
		zc, err := heldZapContext(token)
		if err != nil {
			return nil, err
		}
		return nipcashclient.ConnectZap(ctx, token, zc, sessionOptions()...)
	}
	if err := checkBillProvenance(tok); err != nil {
		return nil, err
	}
	return nipcashclient.Connect(ctx, token, sessionOptions()...)
}

// isProvenanceRefusal reports dialBill refusing a bill before any network call.
func isProvenanceRefusal(err error) bool {
	return errors.Is(err, errProvenanceFailed) || errors.Is(err, errNeedsZapEvent) || errors.Is(err, errNoProvenance)
}

// returnedBillCheck checks a bill a Hub handed back (a split piece, a merge, a
// moved note) with dialBill's rule, and that its minter is the one the bills it
// came from had, when that is known. A bill that fails is still kept — it may be
// the holder's money — but stored unverified, said so, and never dialled.
func returnedBillCheck(token string, expectMinter *string) (verified bool, minter *string) {
	tok, err := nipcash.Decode(token)
	if err != nil || checkBillProvenance(tok) != nil {
		return false, nil
	}
	got, _ := nipcash.VerifyProvenance(tok)
	if expectMinter != nil && !strings.EqualFold(got, *expectMinter) {
		return false, &got
	}
	return true, &got
}

// unverifiedReturnNote is what a command says about a bill returnedBillCheck
// refused.
const unverifiedReturnNote = "The Hub returned a bill whose mint signature doesn't check out; it is kept but marked unverified, and cashctl will not contact its relays."

// heldZapContext is the zap context of a held zap bill, from the Cash Zap it
// was received with; errNeedsZapEvent without one.
func heldZapContext(token string) (nipcash.ZapContext, error) {
	raw, err := ledger.ZapEventFor(token)
	if err != nil || raw == "" {
		return nipcash.ZapContext{}, errNeedsZapEvent
	}
	var ev nip01.Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		return nipcash.ZapContext{}, errNeedsZapEvent
	}
	z, err := nipCZ.ParseCashZap(&ev)
	if err != nil {
		return nipcash.ZapContext{}, errNeedsZapEvent
	}
	return nipCZ.ZapContextFromEvent(z)
}

// dialZapFn dials a zap bill with the zap context its provenance is checked
// against (rule 9 of a Cash Zap being received). Swapped in tests.
var dialZapFn = func(ctx context.Context, token string, zc nipcash.ZapContext) (zapStatusClient, error) {
	return nipcashclient.ConnectZap(ctx, token, zc, sessionOptions()...)
}
