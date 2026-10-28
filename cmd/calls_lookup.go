package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// Settling calls (NIP-CASH §Looking Up a Call). An open call is looked up with
// its exact request id and deadline, signed by the key that authorized it (a
// cash note's lookup first goes without its secret, and carries it only after
// the Hub answers UNAUTHORIZED — nmilat does the two steps). nmilat's Recover
// turns the answer into an action: done applies the result the Hub kept;
// pending waits for retry_after; unknown re-places the call under its derived
// id (only this device's, at most three times) or, when the Hub had answered it
// with a code, closes it not applied with that code; expired is shown with its
// secrets; an unanswered lookup is never read as unknown.

// errNeedsCredential is a call authorized with --as: the credential is never
// stored, so it cannot be looked up or re-placed without it.
var errNeedsCredential = errors.New("this call was authorized with --as, which cashctl never stores; settle it with the same --as")

// pendingOf is nmilat's view of a ledger call.
func pendingOf(c *ledger.Call) nipcash.PendingCall {
	pc := nipcash.PendingCall{
		RequestID: c.RequestID, Deadline: c.Deadline, Method: c.Method, Target: c.Target,
		HubIdentity: c.HubIdentity, MaxRequestAge: c.SentH, Device: c.DeviceID, Amount: c.AmountMillis,
		Chain: c.Chain, DerivedFrom: c.Replaces, Attempt: c.Attempt, LastCode: c.FirstError,
		LastDetail: nipcash.Detail(c.FirstDetail), SawReused: c.SawReused, Params: json.RawMessage(c.ParamsJSON),
	}
	if c.AuthorizerKind == ledger.AuthCashSecret {
		pc.SourceSecret = c.AuthorizerSecret
	}
	if c.AuthorizerKind == ledger.AuthBuyerKey {
		pc.BuyerKey = c.AuthorizerSecret
	}
	for _, s := range c.Secrets {
		if s.Role == ledger.RoleInterimKey {
			// cashctl's own record, never a secret of the request: it goes to the
			// re-placement through specOf.
			continue
		}
		role := ""
		switch s.Role {
		case ledger.RoleRemainder:
			role = "remainder"
		case ledger.RoleRemint:
			role = nipcash.SecretRoleRemint
		}
		pc.Secrets = append(pc.Secrets, nipcash.PendingSecret{Secret: s.Secret, Role: role})
	}
	return pc
}

// specOf is the context a call was placed with, for its re-placement.
func specOf(c *ledger.Call) callSpec {
	return callSpec{Purpose: c.Purpose, SourceEntryIDs: c.SourceEntryIDs, AuthorizerKind: c.AuthorizerKind,
		AuthorizerPubkey: c.AuthorizerPubkey, AuthorizerSecret: c.AuthorizerSecret, TargetCredential: c.TargetCredential, Chain: c.Chain,
		InterimKey: callSecretOf(c, ledger.RoleInterimKey)}
}

// callCredential is the credential that authorized c, which also signs its
// lookup.
func callCredential(cmd *cobra.Command, c *ledger.Call) (nipcash.Credential, error) {
	switch c.AuthorizerKind {
	case ledger.AuthCashSecret:
		if c.AuthorizerSecret == "" {
			return nil, fmt.Errorf("call %s keeps no source secret", c.CallID)
		}
		return nipcash.BySecret(c.AuthorizerSecret), nil
	case ledger.AuthThrowawayKey:
		if c.AuthorizerSecret == "" {
			return nil, fmt.Errorf("call %s keeps no key", c.CallID)
		}
		return nipcash.BySigning(c.AuthorizerSecret), nil
	case ledger.AuthAsCredential:
		as := asCredentialValue(cmd)
		if as == "" {
			return nil, errNeedsCredential
		}
		return credential.ParseCash(as)
	default:
		return localCashCredential(cmd)
	}
}

// requestShape is the part of a call's request (NIP-CASH wire names) that a
// re-placement or a late result needs.
type requestShape struct {
	NewIdentity struct {
		IdentityType  string `json:"identity_type"`
		IdentityValue string `json:"identity_value"`
		IAPubkey      string `json:"ia_pubkey"`
	} `json:"new_identity"`
	AmountMillis        *uint64 `json:"amount_millis"`
	RemainderCommitment string  `json:"remainder_commitment"`
	Sources             []struct {
		WalletPubkey string `json:"wallet_pubkey"`
	} `json:"sources"`
}

func requestOf(c *ledger.Call) (requestShape, error) {
	var req requestShape
	if err := json.Unmarshal([]byte(c.ParamsJSON), &req); err != nil {
		return req, fmt.Errorf("call %s: unreadable request: %w", c.CallID, err)
	}
	return req, nil
}

func callSecretOf(c *ledger.Call, role string) string {
	for _, s := range c.Secrets {
		if s.Role == role {
			return s.Secret
		}
	}
	return ""
}

// callTarget rebuilds the call's new_identity.
func callTarget(c *ledger.Call, req requestShape) (nipcash.Target, error) {
	switch req.NewIdentity.IdentityType {
	case "cash":
		secret := callSecretOf(c, ledger.RoleNewIdentity)
		if secret == "" {
			return nil, fmt.Errorf("call %s keeps no new cash secret", c.CallID)
		}
		return nipcash.CashTargetFromSecret(secret)
	case "pubkey":
		return nipcash.Pubkey(req.NewIdentity.IdentityValue), nil
	case "connection_key":
		return nipcash.ResolvedConnectionKey(nipIC.ConnectionKey(req.NewIdentity.IdentityValue), "", req.NewIdentity.IAPubkey)
	}
	return nil, fmt.Errorf("call %s: unknown new identity %q", c.CallID, req.NewIdentity.IdentityType)
}

// transferParams rebuilds a cash_transfer call's params.
func transferParams(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call) (nipcash.CashTransferParams, error) {
	req, err := requestOf(c)
	if err != nil {
		return nipcash.CashTransferParams{}, err
	}
	cred, err := callCredential(cmd, c)
	if err != nil {
		return nipcash.CashTransferParams{}, err
	}
	to, err := callTarget(c, req)
	if err != nil {
		return nipcash.CashTransferParams{}, err
	}
	p := nipcash.CashTransferParams{Credential: cred, To: to, CurrentAmount: c.AmountMillis, RequestID: c.RequestID}
	if req.AmountMillis != nil {
		split := *req.AmountMillis
		p.SplitAmount = &split
		if len(c.SourceEntryIDs) > 0 {
			if e, ok := l.Find(c.SourceEntryIDs[0]); ok && e.AmountMillis != nil {
				p.CurrentAmount = *e.AmountMillis
			}
		}
	}
	if req.RemainderCommitment != "" {
		rt, err := nipcash.CashTargetFromSecret(callSecretOf(c, ledger.RoleRemainder))
		if err != nil {
			return p, fmt.Errorf("call %s keeps no remainder secret: %w", c.CallID, err)
		}
		p.RemainderTarget = rt
	}
	return p, nil
}

// consolidateParams rebuilds a cash_consolidate call's params, in the order sent.
func consolidateParams(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call) (nipcash.CashConsolidateParams, error) {
	req, err := requestOf(c)
	if err != nil {
		return nipcash.CashConsolidateParams{}, err
	}
	to, err := callTarget(c, req)
	if err != nil {
		return nipcash.CashConsolidateParams{}, err
	}
	p := nipcash.CashConsolidateParams{To: to, RequestID: c.RequestID}
	for _, s := range req.Sources {
		e := firstSourceEntry(l, c.SourceEntryIDs, s.WalletPubkey)
		if e == nil || e.AmountMillis == nil {
			return p, fmt.Errorf("call %s: source %s is not held", c.CallID, s.WalletPubkey)
		}
		cred, err := resolveCredential(cmd, e)
		if err != nil {
			return p, err
		}
		p.Sources = append(p.Sources, nipcash.Source{WalletPubkey: e.WalletPubkey, Amount: *e.AmountMillis, Credential: cred})
	}
	if len(p.Sources) > 0 && c.AuthorizerKind == ledger.AuthAsCredential {
		cred, err := callCredential(cmd, c)
		if err != nil {
			return p, err
		}
		p.Sources[0].Credential = cred
	}
	return p, nil
}

// lookupCallFn looks c up. Swapped in tests.
var lookupCallFn = func(ctx context.Context, cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call) (nipcashclient.LookupOutcome, error) {
	if c.Method == nipcash.MethodMintCash {
		// Looked up over the Hub connection it was placed on, signed with its key.
		hub, err := dialCashHubFn(ctx, c.TargetCredential)
		if err != nil {
			return nipcashclient.LookupOutcome{}, err
		}
		defer hub.Close()
		return hub.LookupMint(ctx, pendingOf(c))
	}
	cred, err := callCredential(cmd, c)
	if err != nil {
		return nipcashclient.LookupOutcome{}, err
	}
	client, err := dialBill(ctx, c.TargetCredential)
	if err != nil {
		return nipcashclient.LookupOutcome{}, err
	}
	defer client.Close()
	if c.Method == nipcash.MethodCashConsolidate {
		return client.LookupConsolidate(ctx, pendingOf(c), cred)
	}
	return client.LookupTransfer(ctx, pendingOf(c), cred)
}

// settledCall is one call settleCalls looked at.
type settledCall struct {
	Call     ledger.Call
	NextStep string
	Summary  string
}

// settleOptions tune settleCalls.
type settleOptions struct {
	// all looks up every open call, due or not.
	all bool
}

// due reports a call worth asking about now.
func due(c *ledger.Call, now int64) bool {
	if !c.Open() || c.State == ledger.CallExpired || c.State == ledger.CallOpenOrder {
		// An open order is read by `cash order show|watch|settle` and `cash
		// settle`, not before every money command.
		return false
	}
	if c.RetryAfter != nil {
		return now >= *c.RetryAfter
	}
	return now > c.Deadline
}

// settleCalls looks up the open calls that are due and acts on each answer,
// saving after each. Never resends a call under its own id; a re-placement goes
// under the derived id.
func settleCalls(cmd *cobra.Command, l *ledger.Ledger, opts settleOptions) []settledCall {
	device, _ := deviceIDFn()
	var out []settledCall
	var ids []string
	for _, c := range l.Calls {
		ids = append(ids, c.CallID)
	}
	for _, id := range ids {
		c, ok := l.FindCall(id)
		if !ok || !c.Open() {
			continue
		}
		if !opts.all && !due(c, time.Now().Unix()) {
			continue
		}
		var summary string
		switch c.Method {
		case nipcash.MethodCashOrder:
			summary = settleOrderCall(cmd, l, c, device)
		case nipcash.MethodCashWithdraw:
			summary = settleWithdrawCall(cmd, l, c, device)
		default:
			summary = settleOne(cmd, l, c, device)
		}
		if c, ok = l.FindCall(id); ok {
			out = append(out, settledCall{Call: *c, Summary: summary, NextStep: nextStep(c, device)})
		}
	}
	return out
}

// settleOne asks about one call and records what the answer means.
func settleOne(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, device string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	id := c.CallID
	action, err := nipcashclient.Recover(ctx, pendingOf(c), func(ctx context.Context, _ nipcash.PendingCall) (nipcashclient.LookupOutcome, error) {
		return lookupCallFn(ctx, cmd, l, c)
	}, device, time.Now())
	c, _ = l.FindCall(id)
	now := time.Now().Unix()
	switch {
	case errors.Is(err, errNeedsCredential):
		c.State, c.Detail = ledger.CallStuck, "needs_credential"
	case err != nil:
		// No answer at all (the Hub unreachable): nothing is decided.
		c.Detail = "lookup failed: " + output.Sanitize(err.Error())
		if now > c.Deadline+c.SentH+600 {
			c.State, c.Detail = ledger.CallStuck, "lookup_unanswered"
		}
	default:
		return applyAction(cmd, l, c, action, device)
	}
	c.UpdatedAt = now
	_ = l.Save()
	return ""
}

// applyAction records a Recover action on c.
func applyAction(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, action nipcashclient.Action, device string) string {
	now := time.Now().Unix()
	c.UpdatedAt = now
	summary := ""
	switch action.Kind {
	case nipcashclient.ActionClose:
		if action.Reason == "derived_already_placed" {
			c.State, c.Detail = ledger.CallReplaced, action.Reason
			break
		}
		s, err := applyCallResult(cmd, l, c, action.Result)
		if err != nil {
			// A result that does not check out keeps the call and its secrets.
			c.State, c.Detail = ledger.CallStuck, "invalid_result"
			break
		}
		summary = s
		c, _ = l.FindCall(c.CallID)
		closeCall(l, c, ledger.CallDone, "")
	case nipcashclient.ActionWait:
		// pending with a retry_after; with none, the lookup went unanswered and
		// is asked again at the next settle.
		if action.Until > 0 {
			until := action.Until
			c.State, c.RetryAfter = ledger.CallPending, &until
		}
	case nipcashclient.ActionShow:
		switch action.Reason {
		case "lookup_expired":
			// The Hub no longer keeps a record: never re-placed, its secrets kept.
			if c.DeviceID == device {
				c.State = ledger.CallExpired
			}
			c.Detail = "expired"
			releaseZapClaim(l, c)
		case "foreign_device":
			c.Detail = "placed from another copy of this wallet"
		default:
			c.State, c.Detail = ledger.CallStuck, action.Reason
		}
	case nipcashclient.ActionReplace:
		summary = replaceCall(cmd, l, c)
	case nipcashclient.ActionConfirmReplace:
		c.State, c.Detail = ledger.CallStuck, "confirm_replace"
	case nipcashclient.ActionCloseFailed:
		releaseZapClaim(l, c)
		closeCall(l, c, ledger.CallNotApplied, "never committed")
	case nipcashclient.ActionCloseRefused:
		releaseZapClaim(l, c)
		closeCall(l, c, ledger.CallNotApplied, action.Reason)
		summary = fmt.Sprintf("the Hub refused it (%s); nothing moved", action.Reason)
	}
	_ = l.Save()
	return summary
}

// releaseZapClaim frees the zap bill of a claim settle closed with nothing
// moved (or whose record the Hub no longer keeps): the entry leaves
// zap_claiming, so receiving the zap again reads its Hub afresh (Z-2).
func releaseZapClaim(l *ledger.Ledger, c *ledger.Call) {
	if c.Purpose != purposeZapClaim {
		return
	}
	for _, id := range c.SourceEntryIDs {
		if e, ok := l.Find(id); ok && e.Status == ledger.StatusZapClaiming {
			_ = l.SetStatus(id, ledger.StatusNotReceived)
		}
	}
}

// replaceCall re-places c under its derived id after the Hub said it never saw
// it, with the same target, amounts and secrets, fresh proofs and a new
// deadline; then c is replaced and the derived call carries on.
func replaceCall(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call) string {
	return replaceCallConfirmed(cmd, l, c, false)
}

// replaceCallConfirmed is replaceCall; confirmed is the user's explicit
// `cash settle --replace`, which a mint needs.
func replaceCallConfirmed(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, confirmed bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	j := &ledgerJournal{l: l, spec: specOf(c)}
	device, _ := deviceIDFn()
	parentID := c.CallID
	var tres *nipcash.CashTransferResult
	var cres *nipcash.CashConsolidateResult
	var mres *nipcash.MintCashResult
	var wres *nipcash.Payout
	var sendErr error
	pc := pendingOf(c)
	if c.Method == nipcash.MethodCashWithdraw {
		// nmilat re-places a withdrawal only with its quote, address and notify.
		if w, ok, _ := ledger.WithdrawalForCall(c.CallID); ok {
			pc = withdrawPending(c, w)
		}
	}
	act, err := nipcashclient.Replace(callContext(ctx), pc, j, nipcashclient.ReplaceOptions{
		Confirmed: confirmed,
		DerivedEntryExists: func(_ context.Context, id string) (bool, error) {
			_, ok := l.FindCall(ledger.CallIDFor(id))
			return ok, nil
		},
	}, func(ctx context.Context, next nipcash.PendingCall) error {
		parent, _ := l.FindCall(parentID)
		if parent.Method == nipcash.MethodCashWithdraw {
			w, _, _ := ledger.WithdrawalForCall(parentID)
			bill, err := dialWithdrawFn(ctx, parent.TargetCredential)
			if err != nil {
				sendErr = &notSentError{err}
				return sendErr
			}
			defer bill.Close()
			q, err := nipcash.ParseWithdrawQuote(w.QuoteJSON)
			if err != nil {
				sendErr = err
				return err
			}
			cred, err := callCredential(cmd, parent)
			if err != nil {
				sendErr = err
				return err
			}
			p := nipcash.CashWithdrawParams{Quote: q, Credential: cred, RequestID: next.RequestID, Notify: w.Notify}
			if s := callSecretOf(parent, ledger.RoleRemint); s != "" {
				if p.Remint, err = nipcash.CashTargetFromSecret(s); err != nil {
					sendErr = err
					return err
				}
			}
			wj := &withdrawJournal{ledgerJournal: *j, w: w}
			wres, sendErr = bill.Withdraw(ctx, p, wj, device)
			j.callIDs = wj.callIDs
			return sendErr
		}
		if parent.Method == nipcash.MethodMintCash {
			hub, err := dialCashHubFn(ctx, parent.TargetCredential)
			if err != nil {
				sendErr = &notSentError{err}
				return sendErr
			}
			defer hub.Close()
			p, err := mintParams(parent)
			if err != nil {
				sendErr = err
				return err
			}
			p.RequestID = next.RequestID
			mres, sendErr = hub.MintCash(ctx, p, j, device)
			return sendErr
		}
		caller, err := dialCallerFn(ctx, parent.TargetCredential)
		if err != nil {
			sendErr = &notSentError{err}
			return sendErr
		}
		defer caller.Close()
		switch parent.Method {
		case nipcash.MethodCashConsolidate:
			p, err := consolidateParams(cmd, l, parent)
			if err != nil {
				sendErr = err
				return err
			}
			p.RequestID = next.RequestID
			cres, sendErr = caller.CashConsolidate(ctx, p, j, device)
		default:
			p, err := transferParams(cmd, l, parent)
			if err != nil {
				sendErr = err
				return err
			}
			p.RequestID = next.RequestID
			tres, sendErr = caller.CashTransfer(ctx, p, j, device)
		}
		return sendErr
	})
	parent, _ := l.FindCall(parentID)
	derivedID := ""
	if child := j.call(); child != nil {
		derivedID = child.CallID
	}
	if sendErr != nil && sentNothing(sendErr) {
		// Nothing reached the Hub: the derived call and its id are dropped, so
		// a later settle can place it again under the same derived id, and the
		// parent stays as it was.
		if derivedID != "" {
			// A withdrawal's row follows its call: it goes back to the parent.
			if w, ok, _ := ledger.WithdrawalForCall(derivedID); ok {
				w.CallID = parentID
				_ = ledger.PutWithdrawal(w)
			}
			l.RemoveCall(derivedID)
		}
		for _, id := range j.derived {
			_ = ledger.RemoveDerived(id)
		}
		parent.Detail = "re-placement not sent: " + output.Sanitize(fmt.Sprint(sendErr))
		_ = l.Save()
		return "not placed again (" + output.Sanitize(fmt.Sprint(sendErr)) + "); it is tried again at the next settle"
	}
	switch {
	case err == nil && act.Kind == nipcashclient.ActionShow:
		parent.State, parent.Detail = ledger.CallStuck, act.Reason
		return ""
	case err == nil && act.Kind == nipcashclient.ActionClose:
		parent.State, parent.Detail = ledger.CallReplaced, act.Reason
		return ""
	case derivedID == "":
		// Nothing written: the parent stays as it was, to be tried again.
		parent.Detail = "re-placement not sent: " + output.Sanitize(fmt.Sprint(err))
		return ""
	}
	parent.State, parent.Detail = ledger.CallReplaced, "replaced by call "+derivedID
	child, _ := l.FindCall(derivedID)
	switch {
	case sendErr == nil && wres != nil:
		w, _, _ := ledger.WithdrawalForCall(derivedID)
		child, _ = l.FindCall(derivedID)
		if err := applyPayout(cmd, l, child, &w, wres); err != nil {
			return "placed again as call " + derivedID
		}
		return "sent again as call " + derivedID + ": " + withdrawalText(w)
	case sendErr == nil && (tres != nil || cres != nil || mres != nil):
		var summary string
		var aErr error
		switch {
		case tres != nil:
			summary, aErr = applyTransferResult(cmd, l, child, tres)
		case mres != nil:
			var out mintOutcome
			out, aErr = applyMintResult(cmd, l, child, mres)
			summary = "minted: " + out.Summary
		default:
			summary, aErr = applyConsolidateResult(cmd, l, child, cres)
		}
		child, _ = l.FindCall(derivedID)
		if aErr != nil {
			child.State, child.Detail = ledger.CallStuck, "invalid_result"
			return ""
		}
		closeCall(l, child, ledger.CallDone, "")
		return "placed again as call " + derivedID + ": " + summary
	default:
		// The re-placement's own outcome follows the placement rules.
		_ = placeErrFor(cmd, l, j, sendErr, transferVerb(child.Purpose), tombstoneEntryID(l, child), child.SourceEntryIDs...)
		finishRefusedWithdrawal(l, derivedID)
		if child.Method == nipcash.MethodCashWithdraw && len(child.SourceEntryIDs) > 0 {
			followOtherCopysWithdrawal(cmd, l, child.SourceEntryIDs[0], sendErr)
		}
		return "placed again as call " + derivedID
	}
}

// tombstoneEntryID is the ledger entry a tombstone answering c is about: the
// call's target bill (a consolidate's wire sources[0], NIP-CASH §Answer or
// Omit), looked up among its sources by wallet, or "" when that bill is not a
// ledger entry. Other methods name their one source first.
func tombstoneEntryID(l *ledger.Ledger, c *ledger.Call) string {
	if c == nil || len(c.SourceEntryIDs) == 0 {
		return ""
	}
	if c.Method != nipcash.MethodCashConsolidate {
		return c.SourceEntryIDs[0]
	}
	if e := firstSourceEntry(l, c.SourceEntryIDs, c.Target); e != nil {
		return e.ID
	}
	return ""
}

// applyCallResult applies the result a lookup returned for c.
func applyCallResult(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, raw json.RawMessage) (string, error) {
	if c.Method == nipcash.MethodMintCash {
		return applyMintLookup(cmd, l, c, raw)
	}
	if c.Method == nipcash.MethodCashConsolidate {
		p, err := consolidateParams(cmd, l, c)
		if err != nil {
			return "", err
		}
		res, err := p.ParseLookupResult(raw)
		if err != nil {
			return "", err
		}
		return applyConsolidateResult(cmd, l, c, res)
	}
	p, err := transferParams(cmd, l, c)
	if err != nil {
		return "", err
	}
	res, err := p.ParseLookupResult(raw)
	if err != nil {
		return "", err
	}
	return applyTransferResult(cmd, l, c, res)
}

// applyTransferResult records a cash_transfer that is known to have happened.
func applyTransferResult(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, res *nipcash.CashTransferResult) (string, error) {
	if len(c.SourceEntryIDs) == 0 {
		return "", fmt.Errorf("call %s names no source", c.CallID)
	}
	e, ok := l.Find(c.SourceEntryIDs[0])
	if !ok {
		return "", fmt.Errorf("call %s: source %s is gone", c.CallID, c.SourceEntryIDs[0])
	}
	newSecret := callSecretOf(c, ledger.RoleNewIdentity)
	// A created bill already destroyed comes back as its tombstone in place of
	// its token (NIP-CASH §Looking Up a Call): the move happened, but no bill is
	// left to hold, so none is recorded.
	if t := res.NewWalletTombstone; t != nil {
		switch c.Purpose {
		case purposeProtect, purposeMergeMove, purposeUnmergeMove:
			_ = l.SetStatus(e.ID, ledger.StatusSpent)
			l.AppendHistory("spent", fmt.Sprintf("token %s was moved, and its Hub says the moved bill is already spent (%s)", e.ID, tombstoneReason(t)))
			return "moved, but the moved bill is already spent (" + tombstoneReason(t) + ")", nil
		}
	}
	switch c.Purpose {
	case purposeZapClaim:
		if summary, notZap := applyZapClaimResult(l, e, c.AmountMillis, res); notZap != "" {
			return summary, nil
		}
		l.AppendHistory("receive", fmt.Sprintf("received a %s Cash Zap to a connection key (settled after its answer was lost)", output.FormatAmount(int64(res.AmountMillis)))) //nolint:gosec // amounts fit
		return "zap claimed", nil
	case purposeProtect:
		followMove(e, res)
		e.CashSecret = newSecret
		e.CashProtection = ledger.CashProtected
		l.AppendHistory("secure", "re-keyed this cash so the shared secret can no longer spend it (settled after its answer was lost)")
		return "protected", nil
	case purposeMergeMove:
		followMove(e, res)
		if e.PendingThrowawayKey == "" {
			return "", fmt.Errorf("call %s: the merge key is not on the bill", c.CallID)
		}
		promoteThrowaway(e)
		return "moved for a merge", nil
	case purposeUnmergeMove:
		followMove(e, res)
		becomeCashNote(e, newSecret)
		return "turned back into a cash note", nil
	}
	// A transfer or split to someone (or to a cash note to hand out).
	_ = l.SetStatus(e.ID, ledger.StatusTransferred)
	if res.RemainderWalletToken != "" {
		remainder := credentialModeOf(*e)
		remainder.MinterPubkey = e.MinterPubkey
		if rs := callSecretOf(c, ledger.RoleRemainder); rs != "" {
			remainder.CashSecret, remainder.CashProtection = rs, ledger.CashProtected
		}
		remainder.Token, remainder.WalletPubkey = strings.ToLower(res.RemainderWalletToken), res.RemainderWalletPubkey
		remainder.Verified, remainder.MinterPubkey = returnedBillCheck(remainder.Token, e.MinterPubkey)
		if res.RemainingAmountMillis != nil && *res.RemainingAmountMillis <= maxSaneAmountMillis {
			remainder.AmountMillis = res.RemainingAmountMillis
		}
		_, _ = l.Add(remainder)
	}
	if t := res.RemainderWalletTombstone; t != nil {
		l.AppendHistory("spent", fmt.Sprintf("the change of token %s is already spent, its Hub says (%s)", e.ID, tombstoneReason(t)))
	}
	sent := res.AmountMillis
	if t := res.NewWalletTombstone; t != nil {
		l.AppendHistory("transfer", fmt.Sprintf("transferred %s; the recipient's bill is already spent (%s) (settled after its answer was lost)", output.FormatAmount(int64(sent)), tombstoneReason(t))) //nolint:gosec // amounts fit
		return "transferred; the recipient's bill is already spent (" + tombstoneReason(t) + ")", nil
	}
	recipient, err := res.RecipientToken(e.Token)
	if err != nil {
		recipient = ""
	}
	if newSecret != "" && recipient != "" {
		note := recipient + "#" + newSecret
		l.AddHandoff(ledger.Handoff{Kind: ledger.HandoffCashNote, Handoff: note, AmountMillis: sent, Target: "cash", WalletPubkey: res.NewWalletPubkey})
		l.AppendHistory("transfer", fmt.Sprintf("transferred %s as cash (settled after its answer was lost)", output.FormatAmount(int64(sent)))) //nolint:gosec // amounts fit
		return "sent as a cash note, kept in `cashctl wallet history`", nil
	}
	if recipient != "" {
		l.AddHandoff(ledger.Handoff{Kind: ledger.HandoffGift, Handoff: recipient, AmountMillis: sent, Target: "identity", WalletPubkey: res.NewWalletPubkey})
	}
	l.AppendHistory("transfer", fmt.Sprintf("transferred %s (settled after its answer was lost)", output.FormatAmount(int64(sent)))) //nolint:gosec // amounts fit
	return "transferred, the recipient's token kept in `cashctl wallet history`", nil
}

// applyConsolidateResult records a cash_consolidate that is known to have
// happened.
func applyConsolidateResult(cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call, res *nipcash.CashConsolidateResult) (string, error) {
	if res.NewWalletToken == "" && res.NewWalletTombstone == nil {
		return "", fmt.Errorf("call %s: the merged bill carries no token", c.CallID)
	}
	for _, id := range c.SourceEntryIDs {
		_ = l.SetStatus(id, ledger.StatusConsolidated)
	}
	// The merged bill was destroyed since (NIP-CASH §Looking Up a Call: its
	// tombstone stands in place of its token): the sources are gone, and no bill
	// is left to hold or hand over.
	if t := res.NewWalletTombstone; t != nil {
		l.AppendHistory("consolidate", fmt.Sprintf("consolidated %d tokens; the merged bill is already spent (%s) (settled after its answer was lost)", len(c.SourceEntryIDs), tombstoneReason(t)))
		return "merged, but the merged bill is already spent (" + tombstoneReason(t) + ")", nil
	}
	verified, minter := returnedBillCheck(res.NewWalletToken, sharedMinterOfIDs(l, c.SourceEntryIDs))
	entry := ledger.Entry{Token: strings.ToLower(res.NewWalletToken), WalletPubkey: res.NewWalletPubkey,
		AmountMillis: &res.AmountMillis, Verified: verified, MinterPubkey: minter, ExpiresAt: expiryPtr(res.ExpiresAt)}
	req, _ := requestOf(c)
	switch {
	case req.NewIdentity.IdentityType == "cash":
		entry.CashSecret, entry.IdentityRequired = callSecretOf(c, ledger.RoleNewIdentity), ptrTo(false)
		entry.CashProtection = ledger.CashProtected
	case req.NewIdentity.IdentityType == "pubkey" && c.AuthorizerKind == ledger.AuthThrowawayKey && pubOf(c.AuthorizerSecret) == req.NewIdentity.IdentityValue:
		entry.IdentityRequired, entry.ThrowawayKey = ptrTo(true), c.AuthorizerSecret
	case req.NewIdentity.IdentityType == "pubkey" && callSecretOf(c, ledger.RoleInterimKey) != "" && pubOf(callSecretOf(c, ledger.RoleInterimKey)) == req.NewIdentity.IdentityValue:
		// An interim round to the merge key, whatever its first source was.
		entry.IdentityRequired, entry.ThrowawayKey = ptrTo(true), callSecretOf(c, ledger.RoleInterimKey)
	case req.NewIdentity.IdentityType == "pubkey" && localPubIs(cmd, req.NewIdentity.IdentityValue):
		entry.IdentityRequired = ptrTo(true)
	default:
		l.AddHandoff(ledger.Handoff{Kind: ledger.HandoffGift, Handoff: entry.Token, AmountMillis: res.AmountMillis, Target: "identity", WalletPubkey: res.NewWalletPubkey})
		return "merged and sent; the recipient's token kept in `cashctl wallet history`", nil
	}
	if _, err := l.Add(entry); err != nil && !errors.Is(err, ledger.ErrAlreadyHeld) {
		return "", err
	}
	l.AppendHistory("consolidate", fmt.Sprintf("consolidated %d tokens into one %s note (settled after its answer was lost)", len(c.SourceEntryIDs), output.FormatAmount(int64(res.AmountMillis)))) //nolint:gosec // amounts fit
	return "merged", nil
}

// tombstoneReason is a tombstone's reason for people: one of the five, or
// "no reason".
func tombstoneReason(t *nipcash.Tombstone) string {
	if t == nil || t.Reason == "" {
		return "no reason"
	}
	return t.Reason
}

func pubOf(priv string) string {
	pub, _ := utils.GetPublicKey(priv)
	return pub
}

func localPubIs(cmd *cobra.Command, pub string) bool {
	mine, err := localPubKeyHex(cmd)
	return err == nil && mine == pub
}

// nextStep is what the user does about a call now.
func nextStep(c *ledger.Call, device string) string {
	switch {
	case !c.Open():
		return "nothing"
	case c.State == ledger.CallOpenWithdraw:
		return "the payout is followed: `cashctl cash withdraw show`"
	case c.Method == nipcash.MethodCashWithdraw && c.Detail == "confirm_replace":
		return "the Hub says this withdrawal never arrived: `cashctl cash withdraw settle --replace <withdrawal> --yes` sends it again"
	case c.State == ledger.CallOpenOrder:
		return "pay the order, then `cashctl cash order show` collects its bills"
	case c.Method == nipcash.MethodCashOrder && c.Detail == "confirm_replace":
		return "the Hub says this order was never placed: `cashctl cash order settle --replace <order>` places it again (it asks for a new payment)"
	case c.State == ledger.CallExpired:
		return "the Hub no longer keeps a record of it; keep its secrets (shown here) and contact the Hub"
	case c.DeviceID != device:
		return "placed from another copy of this wallet: settle it there"
	case c.Detail == "needs_credential":
		return "run `cashctl cash settle --as <credential>` with the credential it was placed with"
	case c.Detail == "confirm_replace":
		return "the Hub never saw this mint: `cashctl cash settle --replace " + c.CallID + "` mints it again (under its derived id, never twice)"
	case c.State == ledger.CallStuck:
		return "run `cashctl cash settle`; if it stays stuck, keep its secrets and contact the Hub"
	}
	return "run `cashctl cash settle` (it is also settled at the start of every money command)"
}
