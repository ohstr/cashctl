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
// is closed as not sent. A bill with an open call is never spent again until that call is
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
	// purposeZapClaim claims a Cash Zap to a connection key: a full in-place
	// transfer to the local pubkey (NIP-CZ rule 9).
	purposeZapClaim = "zap_claim"
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
	// InterimKey is the merge key whose pubkey the call targets (an interim
	// consolidate round), kept with the call so a lookup can apply it.
	InterimKey string
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
	// derived is every derived id this journal recorded (a re-placement).
	derived []string
}

// WriteAhead records calls, saves and reads each back. Any failure removes
// what it wrote and is returned, so nmilat sends nothing.
func (j *ledgerJournal) WriteAhead(ctx context.Context, calls []nipcash.PendingCall) error {
	var written []string
	undo := func(err error) error {
		j.l.DropStagedOrders()
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
		if c.AuthorizerKind == ledger.AuthBuyerKey && c.AuthorizerSecret == "" {
			c.AuthorizerSecret = pc.BuyerKey
		}
		for _, s := range pc.Secrets {
			role := ledger.RoleNewIdentity
			switch s.Role {
			case "remainder":
				role = ledger.RoleRemainder
			case nipcash.SecretRoleRemint:
				role = ledger.RoleRemint
			}
			c.Secrets = append(c.Secrets, ledger.CallSecret{Role: role, Secret: s.Secret})
		}
		if j.spec.InterimKey != "" {
			c.Secrets = append(c.Secrets, ledger.CallSecret{Role: ledger.RoleInterimKey, Secret: j.spec.InterimKey})
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
	markWrittenAhead(ctx)
	crashpoint.Hit("calls.after_writeahead")
	return nil
}

// RecordDerived records a derived id placed from here.
func (j *ledgerJournal) RecordDerived(_ context.Context, id string, keepUntil int64) error {
	if err := ledger.InsertDerived(id, "", nil, keepUntil); err != nil {
		return err
	}
	j.derived = append(j.derived, id)
	return nil
}

// RecordDerivedFrom records a derived id with the request id it replaces
// (nipcash/client.DerivedParentRecorder): the derived call's row then names
// its parent even when it is written once, through the send path.
func (j *ledgerJournal) RecordDerivedFrom(_ context.Context, id, parent string, keepUntil int64) error {
	if err := ledger.InsertDerived(id, parent, nil, keepUntil); err != nil {
		return err
	}
	j.derived = append(j.derived, id)
	return nil
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

// refuseLocked is refuseOpenCalls plus a bill whose redeem may still settle
// (redeem_in_flight) and a merge key still waiting to be confirmed on the
// bill: such a key may be all that spends it, so nothing may replace or clear
// it.
func refuseLocked(cmd *cobra.Command, l *ledger.Ledger, entryIDs []string) error {
	for _, id := range entryIDs {
		if e, ok := l.Find(id); ok && e.Status == ledger.StatusRedeeming {
			return errRedeemInFlight(cmd, id)
		}
	}
	if err := refuseOpenCalls(cmd, l, entryIDs); err != nil {
		return err
	}
	for _, id := range entryIDs {
		if e, ok := l.Find(id); ok && e.PendingThrowawayKey != "" {
			return output.FinalConflictError(cmd, id, fmt.Errorf(
				"an earlier merge moved %s onto a key the Hub has not confirmed yet, so nothing more is sent from it until it does. "+
					"Run `cashctl cash settle`, then try again", id))
		}
	}
	return nil
}

// locked reports a held bill no new call may spend: an open call names it, or
// a merge key on it is not confirmed yet.
func locked(l *ledger.Ledger, e *ledger.Entry) bool {
	if e.PendingThrowawayKey != "" {
		return true
	}
	_, open := l.OpenCallFor(e.ID)
	return open
}

// unlockedHeld is l.Held() without the bills an automatic pick must skip
// (locked): they stay the user's, but are spoken for until their call ends.
func unlockedHeld(l *ledger.Ledger) []ledger.Entry {
	var out []ledger.Entry
	for _, e := range l.Held() {
		if !locked(l, &e) {
			out = append(out, e)
		}
	}
	return out
}

// pickableHeld is what an automatic pick chooses from: the held bills no open
// call speaks for. With nothing held it is not_found; with every held bill
// locked, the first one's refusal.
func pickableHeld(cmd *cobra.Command, l *ledger.Ledger) ([]ledger.Entry, error) {
	all := l.Held()
	if len(all) == 0 {
		return nil, output.NotFoundError(cmd, "", fmt.Errorf("you have no held cash tokens — receive one first with `cashctl receive <token>`"))
	}
	held := unlockedHeld(l)
	if len(held) == 0 {
		return nil, refuseLocked(cmd, l, []string{all[0].ID})
	}
	return held, nil
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
	if state == ledger.CallNotApplied {
		stageNotPlacedOrder(l, c)
	}
}

// stageNotPlacedOrder ends, in the next Save, the order row of a cash_order
// call that closed without placing (never sent, refused, or a re-placement
// that went nowhere): nothing can be paid or minted for it, so it must not
// stay `placing` behind a closed call (R2-C2).
func stageNotPlacedOrder(l *ledger.Ledger, c *ledger.Call) {
	if l == nil || c.Method != nipcash.MethodCashOrder {
		return
	}
	o, ok, err := ledger.OrderForCall(c.CallID)
	if err != nil || !ok || o.FinishedAt != 0 || o.State != ledger.OrderPlacing {
		return
	}
	now := time.Now().Unix()
	o.State, o.FinishedAt, o.CheckedAt = ledger.OrderNotPlaced, now, now
	l.StageOrder(o)
}

// detailNotSent is the detail of a call written ahead and never published.
const detailNotSent = "not_sent"

// closeNotSent closes a call written ahead that never reached the Hub: kept as
// a record, final, with its secrets wiped.
func closeNotSent(l *ledger.Ledger, c *ledger.Call) {
	if c == nil {
		return
	}
	closeCall(l, c, ledger.CallNotApplied, detailNotSent)
	_ = l.Save()
}

// sentNothing reports a placement error that guarantees nothing reached the
// Hub: the dial failed, or nmilat never published.
func sentNothing(err error) bool {
	var notSent *notSentError
	return errors.As(err, &notSent) || errors.Is(err, nipcashclient.ErrNotSent) || errors.Is(err, nipcashclient.ErrPowTooHard) ||
		errors.Is(err, nipcashclient.ErrHubUnusable)
}

// placeErr turns a placed call's failure into its outcome on the ledger and the
// user's error. j is the journal it was placed with (it knows whether a call
// was written); verb names the action; ids are the source entries.
func placeErr(cmd *cobra.Command, l *ledger.Ledger, j *ledgerJournal, err error, verb string, ids ...string) error {
	billID := ""
	if len(ids) > 0 {
		billID = ids[0]
	}
	return placeErrFor(cmd, l, j, err, verb, billID, ids...)
}

// placeErrFor is placeErr with the entry a tombstone answers for named
// explicitly: billID is the ledger entry of the item's bill (a consolidate's
// wire sources[0], which need not be ids[0]), or "" when that bill is not in
// the ledger, so a tombstone marks nothing spent.
func placeErrFor(cmd *cobra.Command, l *ledger.Ledger, j *ledgerJournal, err error, verb, billID string, ids ...string) error {
	c := j.call()
	input := ""
	if len(ids) == 1 {
		input = ids[0]
	}
	var final *nipcashclient.FinalError
	var gone *nipcash.BillGone
	var indeterminate *nipcashclient.IndeterminateError
	switch {
	case errors.Is(err, nipcashclient.ErrHubUnusable):
		// Refused before writing, or before publishing (with ErrNotSent).
		closeNotSent(l, c)
		return hubUnusableError(cmd, err)
	case errors.Is(err, nipcashclient.ErrPowTooHard):
		closeNotSent(l, c)
		return output.RuntimeError(cmd, fmt.Errorf(
			"not placed — this Hub's relays ask for more proof of work than cashctl does (%s caps it at difficulty %d); your money has not moved: %w",
			powMaxEnv, mustPowMax(), err))
	case errors.Is(err, nipcashclient.ErrNotSent) || (c == nil && errors.Is(err, nipcashclient.ErrNoJournal)):
		// Written ahead but never published, or never written: nothing reached
		// the Hub.
		closeNotSent(l, c)
		return output.RuntimeError(cmd, fmt.Errorf("not placed — your money has not moved: %w", err))
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
		if billID != "" {
			if e, ok := l.Find(billID); ok {
				markGone(l, e, &gone.Tombstone)
			}
		}
		_ = l.Save()
		state := stateForTombstone(&gone.Tombstone)
		cause := goneCause(state)
		switch {
		case billID != "" && len(ids) > 1:
			return goneErrorf(cmd, billID, state, fmt.Errorf("%w (token %s); nothing was %sd, and your other tokens are still yours", cause, billID, verb))
		case billID == "" && len(ids) > 0:
			return goneErrorf(cmd, input, state, fmt.Errorf("%w (the first token given, which is not one cashctl holds); nothing was %sd, and your tokens are still yours", cause, verb))
		}
		return goneErrorf(cmd, input, state, cause)
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
		perr := placeErr(cmd, l, j, err, transferVerb(spec.Purpose), spec.SourceEntryIDs...)
		raw, closedErr := awaitPlacement(ctx, cmd, l, j.call())
		if closedErr != nil {
			return nil, nil, closedErr
		}
		if raw != nil {
			// Looked up done: applied exactly as the reply would have been.
			if r, pErr := p.ParseLookupResult(raw); pErr == nil && r.NewWalletTombstone == nil && r.RemainderWalletTombstone == nil {
				crashpoint.Hit("calls.before_apply")
				return r, j.call(), nil
			}
		}
		return nil, nil, perr
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
		// A tombstone answers for the wire's sources[0], which the dial order
		// or a non-ledger --sources token can put anywhere in, or outside,
		// SourceEntryIDs (NIP-CASH §Answer or Omit).
		billID := ""
		if len(p.Sources) > 0 {
			if e := firstSourceEntry(l, spec.SourceEntryIDs, p.Sources[0].WalletPubkey); e != nil {
				billID = e.ID
			}
		}
		perr := placeErrFor(cmd, l, j, err, "consolidate", billID, spec.SourceEntryIDs...)
		raw, closedErr := awaitPlacement(ctx, cmd, l, j.call())
		if closedErr != nil {
			return nil, nil, closedErr
		}
		if raw != nil {
			if r, pErr := p.ParseLookupResult(raw); pErr == nil && r.NewWalletTombstone == nil {
				crashpoint.Hit("calls.before_apply")
				return r, j.call(), nil
			}
		}
		return nil, nil, perr
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
	case purposeZapClaim:
		return "zap claim"
	}
	return "transfer"
}

// authorizerOf is who a credential is, for a call's record: the kind, the
// signing key's public key, and the secret a cash note or throwaway key
// authorizes with (kept until the call is final, for its lookup).
func authorizerOf(cmd *cobra.Command, e *ledger.Entry, cred nipcash.Credential) (kind, pub, secret string) {
	// An --as (or CASHCTL_AS) override is the credential resolveCredential
	// actually signs the call with, so it — not the entry's own kind — is the
	// authorizer recorded here, read exactly the way proofOf reads it. A cash:
	// secret that IS this entry's own cash secret authorizes as that secret; any
	// other override key is one cashctl cannot tie to the entry (as_credential),
	// whose later NOT_FOUND proves nothing about the held slice and must never
	// mark it reclaimed (R-CP-RECLAIM). This is checked BEFORE the entry-type
	// branches so authorizerOf stays consistent with resolveCredential, which
	// reads --as first: otherwise a throwaway/cash/pubkey slice spent under
	// CASHCTL_AS would be signed by the override yet recorded as the entry's own
	// key, and proofOfCall would reclaim a live slice on the override's NOT_FOUND.
	if as := asCredentialValue(cmd); as != "" {
		if s, ok := strings.CutPrefix(as, "cash:"); ok && e != nil && isCashNote(e) && s == e.CashSecret {
			return ledger.AuthCashSecret, "", e.CashSecret
		}
		return ledger.AuthAsCredential, "", ""
	}
	switch {
	case e != nil && e.ThrowawayKey != "":
		pub, _ = utils.GetPublicKey(e.ThrowawayKey)
		return ledger.AuthThrowawayKey, pub, e.ThrowawayKey
	case e != nil && isCashNote(e):
		return ledger.AuthCashSecret, "", e.CashSecret
	}
	pub, _ = localPubKeyHex(cmd)
	return ledger.AuthLocalIdentity, pub, ""
}
