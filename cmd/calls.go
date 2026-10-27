package cmd

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/crashpoint"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
	"github.com/ohstr/cashctl/internal/store"
)

// Call placement (NIP-CASH §Lost Replies). Every bill-creating call —
// cash_transfer and cash_consolidate — carries a request_id and is written to
// the ledger before it is sent: the id, the exact deadline, the target, who
// authorizes it and every secret it generated, read back on a fresh connection.
// Its outcome then decides the row: a result applies and closes it, BAD_REQUEST
// closes it not applied, a tombstone closes it and marks the bill it names spent, any
// other answer — or none — keeps it open to be looked up, and a call never sent
// is removed. A bill with an open call is never spent again until that call is
// settled.

// Call purposes.
const (
	purposeTransfer    = "transfer"
	purposeSplit       = "split"
	purposeProtect     = "protect"
	purposeMergeMove   = "merge_move"
	purposeUnmergeMove = "unmerge_move"
	purposeMergeRound  = "merge_round"
	purposeConsolidate = "consolidate"
)

// callSpec is what cashctl knows about a call that nmilat's PendingCall does
// not: why it is made, which held bills it spends, who authorizes it, and the
// string it was dialled with.
type callSpec struct {
	Purpose          string
	SourceEntryIDs   []string
	AuthorizerKind   string
	AuthorizerPubkey string
	AuthorizerSecret string
	TargetCredential string
	// Chain is the chain the call's Hub runs on, when the target credential is
	// not a bill whose HRP names it (a mint's cashhub1).
	Chain string
}

// billCaller places bill-creating calls on one dialled bill. *nipcashclient.Client
// is one; tests stand in a fake Hub.
type billCaller interface {
	CashTransfer(ctx context.Context, p nipcash.CashTransferParams, j nipcashclient.Journal, device string) (*nipcash.CashTransferResult, error)
	CashConsolidate(ctx context.Context, p nipcash.CashConsolidateParams, j nipcashclient.Journal, device string) (*nipcash.CashConsolidateResult, error)
	Close()
}

// dialCallerFn dials a bill for a bill-creating call, through the provenance
// gate. Swapped in tests.
var dialCallerFn = func(ctx context.Context, token string) (billCaller, error) {
	c, err := dialBill(ctx, token)
	if err != nil {
		return nil, err
	}
	return c, nil
}

// ledgerJournal is the nmilat Journal over the ledger: each call nmilat is
// about to send becomes a pending_calls row, saved with the ledger's other
// changes and read back before nmilat may publish.
type ledgerJournal struct {
	l       *ledger.Ledger
	spec    callSpec
	callIDs []string
}

// WriteAhead records calls, saves and reads each back. Any failure removes
// what it wrote and is returned, so nmilat sends nothing.
func (j *ledgerJournal) WriteAhead(_ context.Context, calls []nipcash.PendingCall) error {
	var written []string
	undo := func(err error) error {
		for _, id := range written {
			j.l.RemoveCall(id)
		}
		_ = j.l.Save()
		return err
	}
	var rewritten []string
	for _, pc := range calls {
		// The same call written again (a re-placement is written ahead before it
		// is packed, then again with the exact deadline it is sent under):
		// update it in place.
		if existing, ok := j.l.FindCall(ledger.CallIDFor(pc.RequestID)); ok {
			existing.Deadline, existing.SentH, existing.DeviceID = pc.Deadline, pc.MaxRequestAge, pc.Device
			existing.ParamsJSON = string(pc.Params)
			existing.UpdatedAt = time.Now().Unix()
			rewritten = append(rewritten, existing.CallID)
			continue
		}
		if pc.DerivedFrom == "" {
			// A re-placement written once, through the send path: its parent is
			// the one Replace recorded with the derived id.
			if parent, err := ledger.DerivedParent(pc.RequestID); err == nil && parent != "" {
				pc.DerivedFrom = parent
				if p, ok := j.l.FindCall(ledger.CallIDFor(parent)); ok && pc.Attempt <= p.Attempt {
					pc.Attempt = p.Attempt + 1
				}
			}
		}
		c := ledger.Call{
			RequestID: pc.RequestID, Method: pc.Method, Purpose: j.spec.Purpose,
			Deadline: pc.Deadline, SentH: pc.MaxRequestAge, HubIdentity: pc.HubIdentity, Target: pc.Target,
			TargetCredential: j.spec.TargetCredential, DeviceID: pc.Device, AmountMillis: pc.Amount,
			Chain: firstNonEmpty(pc.Chain, j.spec.Chain, chainOfToken(j.spec.TargetCredential)), AuthorizerKind: j.spec.AuthorizerKind,
			AuthorizerPubkey: j.spec.AuthorizerPubkey, AuthorizerSecret: j.spec.AuthorizerSecret,
			SourceEntryIDs: slices.Clone(j.spec.SourceEntryIDs), ParamsJSON: string(pc.Params),
			Attempt: pc.Attempt, Replaces: pc.DerivedFrom,
		}
		if c.AuthorizerKind == ledger.AuthCashSecret && c.AuthorizerSecret == "" {
			c.AuthorizerSecret = pc.SourceSecret
		}
		for _, s := range pc.Secrets {
			role := ledger.RoleNewIdentity
			if s.Role == "remainder" {
				role = ledger.RoleRemainder
			}
			c.Secrets = append(c.Secrets, ledger.CallSecret{Role: role, Secret: s.Secret})
		}
		added, err := j.l.AddCall(c)
		if err != nil {
			return undo(err)
		}
		written = append(written, added.CallID)
	}
	if err := j.l.Save(); err != nil {
		return undo(err)
	}
	for _, id := range append(slices.Clone(written), rewritten...) {
		c, _ := j.l.FindCall(id)
		if err := ledger.VerifyWritten(*c); err != nil {
			return undo(err)
		}
	}
	for _, id := range append(written, rewritten...) {
		if !slices.Contains(j.callIDs, id) {
			j.callIDs = append(j.callIDs, id)
		}
	}
	crashpoint.Hit("calls.after_writeahead")
	return nil
}

// RecordDerived records a derived id placed from here.
func (j *ledgerJournal) RecordDerived(_ context.Context, id string, keepUntil int64) error {
	return ledger.InsertDerived(id, "", nil, keepUntil)
}

// RecordDerivedFrom records a derived id with the request id it replaces
// (nipcash/client.DerivedParentRecorder): the derived call's row then names
// its parent even when it is written once, through the send path.
func (j *ledgerJournal) RecordDerivedFrom(_ context.Context, id, parent string, keepUntil int64) error {
	return ledger.InsertDerived(id, parent, nil, keepUntil)
}

var _ nipcashclient.DerivedParentRecorder = (*ledgerJournal)(nil)

// DerivedPlaced reports a derived id recorded here.
func (j *ledgerJournal) DerivedPlaced(_ context.Context, id string) (bool, error) {
	return ledger.DerivedRecorded(id)
}

// call is the one call this journal wrote, or nil.
func (j *ledgerJournal) call() *ledger.Call {
	if len(j.callIDs) == 0 {
		return nil
	}
	c, ok := j.l.FindCall(j.callIDs[len(j.callIDs)-1])
	if !ok {
		return nil
	}
	return c
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// chainOfToken is the chain a bill's HRP belongs to (NIP-CASH §Mint
// Provenance's table); "" for a prefix the table does not name.
func chainOfToken(token string) string {
	hrp, _, _ := strings.Cut(strings.ToLower(token), "1")
	chain, _ := nipcash.ChainForHRP(hrp)
	return chain
}

// deviceIDFn is this wallet file's device id. Swapped in tests.
var deviceIDFn = store.DeviceID

// errOpenCall is a bill with a call still open: nothing more is placed on it
// until the Hub has said what happened.
func errOpenCall(cmd *cobra.Command, c *ledger.Call, entryID string) error {
	return output.FinalConflictError(cmd, entryID, fmt.Errorf(
		"an earlier %s of %s (call %s) has no answer yet, so nothing more is sent from it until the Hub says what happened. "+
			"Don't hand out its old code again", c.Purpose, entryID, c.CallID))
}

// refuseOpenCalls is the guard before any call: a source with an open call is
// refused (the B4 invariant, kept by the ledger rather than by entry fields).
func refuseOpenCalls(cmd *cobra.Command, l *ledger.Ledger, entryIDs []string) error {
	for _, id := range entryIDs {
		if c, ok := l.OpenCallFor(id); ok {
			return errOpenCall(cmd, c, id)
		}
	}
	return nil
}

// closeCall gives a call its final state: the generated secrets have been
// copied into an entry or a hand-off by the caller, so they and the call's
// authorizing secrets are wiped. The caller's Save persists it with the
// entries it changed, in one transaction.
func closeCall(l *ledger.Ledger, c *ledger.Call, state, detail string) {
	if c == nil {
		return
	}
	c.State, c.Detail, c.UpdatedAt = state, detail, time.Now().Unix()
	c.Secrets = nil
	c.AuthorizerSecret, c.TargetCredential = "", ""
}

// placeErr turns a placed call's failure into its outcome on the ledger and the
// user's error. j is the journal it was placed with (it knows whether a call
// was written); verb names the action; ids are the source entries.
func placeErr(cmd *cobra.Command, l *ledger.Ledger, j *ledgerJournal, err error, verb string, ids ...string) error {
	c := j.call()
	input := ""
	if len(ids) == 1 {
		input = ids[0]
	}
	var final *nipcashclient.FinalError
	var gone *nipcash.BillGone
	var indeterminate *nipcashclient.IndeterminateError
	switch {
	case errors.Is(err, nipcashclient.ErrPowTooHard):
		if c != nil {
			l.RemoveCall(c.CallID)
			_ = l.Save()
		}
		return output.RuntimeError(cmd, fmt.Errorf(
			"not placed — this Hub's relays ask for more proof of work than cashctl does (%s caps it at difficulty %d); your money has not moved: %w",
			powMaxEnv, mustPowMax(), err))
	case errors.Is(err, nipcashclient.ErrNotSent) || (c == nil && errors.Is(err, nipcashclient.ErrNoJournal)):
		// Written ahead but never published, or never written: nothing reached
		// the Hub.
		if c != nil {
			l.RemoveCall(c.CallID)
			_ = l.Save()
		}
		return output.RuntimeError(cmd, fmt.Errorf("not placed — your money has not moved: %w", err))
	case errors.Is(err, nipcashclient.ErrHubUnusable):
		return output.RuntimeError(cmd, fmt.Errorf(
			"this Hub does not announce a request window, so cashctl will not send it a call that creates a bill — your money has not moved: %w", err))
	case errors.As(err, &final):
		closeCall(l, c, ledger.CallNotApplied, final.Code)
		_ = l.Save()
		msg := final.Message
		if msg == "" {
			msg = final.Code
		}
		return output.InvalidInputError(cmd, input, fmt.Errorf("the Hub refused this %s as malformed (%s), so nothing moved", verb, output.Sanitize(msg)))
	case errors.As(err, &gone):
		// The tombstone answers for the one bill the item names: a transfer's
		// source, or a consolidate's sources[0]. Every other source is a live
		// bill the Hub never touched, so it stays held (NIP-CASH §Answer or Omit).
		closeCall(l, c, ledger.CallNotApplied, "tombstone")
		if len(ids) == 0 {
			_ = l.Save()
			return output.SpentError(cmd, input, errBillSpent)
		}
		if e, ok := l.Find(ids[0]); ok {
			markSpent(l, e)
		}
		_ = l.Save()
		if len(ids) > 1 {
			return output.SpentError(cmd, ids[0], fmt.Errorf("%w (token %s); nothing was %sd, and your other tokens are still yours", errBillSpent, ids[0], verb))
		}
		return output.SpentError(cmd, input, errBillSpent)
	case c != nil && errors.As(err, &indeterminate):
		c.State, c.UpdatedAt = ledger.CallLookup, time.Now().Unix()
		if indeterminate.Code != "" {
			c.FirstError = indeterminate.Code
			c.FirstDetail = indeterminate.Detail
			c.Detail = indeterminate.Code
		} else {
			c.Detail = "no answer"
		}
		c.SawReused = c.SawReused || indeterminate.Call.SawReused
		_ = l.Save()
		return callIndeterminateError(cmd, input, c, verb, err)
	case c != nil:
		// Written ahead and the outcome is not one of the above: never assume
		// it failed. It stays open and is looked up.
		c.State, c.Detail, c.UpdatedAt = ledger.CallLookup, "no answer", time.Now().Unix()
		_ = l.Save()
		return callIndeterminateError(cmd, input, c, verb, err)
	}
	// No call was written: nothing can have reached the Hub.
	var notSent *notSentError
	if errors.As(err, &notSent) || isRejectedBeforeSend(err) {
		return classifyCashSpendErr(cmd, err, verb, ids...)
	}
	return output.RuntimeError(cmd, fmt.Errorf("not placed — your money has not moved: %w", err))
}

// callIndeterminateError is exit 8 for a call whose outcome is unknown: it
// stays open, and the bill it spends is not used again until it is settled.
func callIndeterminateError(cmd *cobra.Command, input string, c *ledger.Call, verb string, cause error) error {
	answered := "gave no answer"
	if c.FirstError != "" {
		answered = "answered " + c.FirstError + ", which does not say whether it was applied"
	}
	device, _ := deviceIDFn()
	call := map[string]any{"call": c.CallID, "method": c.Method, "state": c.State, "detail": c.Detail,
		"deadline": c.Deadline, "settle_after": c.Deadline + 1, "next_step": nextStep(c, device)}
	return output.WithCall(output.IndeterminateError(cmd, input, fmt.Errorf(
		"the Hub %s to this %s (call %s), so whether it moved is unknown. "+
			"Nothing is resent: `cashctl cash settle` asks the Hub what happened, and every money command does before this bill is used again: %w",
		answered, verb, c.CallID, cause)), call)
}

// placeTransfer places one cash_transfer on caller, written ahead under spec,
// after the open-call guard. On success the caller applies the result and
// closes the returned call before its own Save.
func placeTransfer(ctx context.Context, cmd *cobra.Command, l *ledger.Ledger, caller billCaller, spec callSpec, p nipcash.CashTransferParams) (*nipcash.CashTransferResult, *ledger.Call, error) {
	if err := refuseOpenCalls(cmd, l, spec.SourceEntryIDs); err != nil {
		return nil, nil, err
	}
	device, err := deviceIDFn()
	if err != nil {
		return nil, nil, output.RuntimeError(cmd, err)
	}
	j := &ledgerJournal{l: l, spec: spec}
	res, err := caller.CashTransfer(callContext(ctx), p, j, device)
	if err != nil {
		return nil, nil, placeErr(cmd, l, j, err, transferVerb(spec.Purpose), spec.SourceEntryIDs...)
	}
	crashpoint.Hit("calls.before_apply")
	return res, j.call(), nil
}

// placeConsolidate is placeTransfer for cash_consolidate.
func placeConsolidate(ctx context.Context, cmd *cobra.Command, l *ledger.Ledger, caller billCaller, spec callSpec, p nipcash.CashConsolidateParams) (*nipcash.CashConsolidateResult, *ledger.Call, error) {
	if err := refuseOpenCalls(cmd, l, spec.SourceEntryIDs); err != nil {
		return nil, nil, err
	}
	device, err := deviceIDFn()
	if err != nil {
		return nil, nil, output.RuntimeError(cmd, err)
	}
	j := &ledgerJournal{l: l, spec: spec}
	res, err := caller.CashConsolidate(callContext(ctx), p, j, device)
	if err != nil {
		return nil, nil, placeErr(cmd, l, j, err, "consolidate", spec.SourceEntryIDs...)
	}
	crashpoint.Hit("calls.before_apply")
	return res, j.call(), nil
}

func transferVerb(purpose string) string {
	switch purpose {
	case purposeProtect:
		return "protect"
	case purposeMergeMove, purposeUnmergeMove:
		return "merge"
	}
	return "transfer"
}

// authorizerOf is who a credential is, for a call's record: the kind, the
// signing key's public key, and the secret a cash note or throwaway key
// authorizes with (kept until the call is final, for its lookup).
func authorizerOf(cmd *cobra.Command, e *ledger.Entry, cred nipcash.Credential) (kind, pub, secret string) {
	switch {
	case e != nil && e.ThrowawayKey != "":
		pub, _ = utils.GetPublicKey(e.ThrowawayKey)
		return ledger.AuthThrowawayKey, pub, e.ThrowawayKey
	case e != nil && isCashNote(e):
		return ledger.AuthCashSecret, "", e.CashSecret
	}
	if as, _ := cmd.Flags().GetString("as"); as != "" {
		return ledger.AuthAsCredential, "", ""
	}
	pub, _ = localPubKeyHex(cmd)
	return ledger.AuthLocalIdentity, pub, ""
}
