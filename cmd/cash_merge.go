package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/ohstr/nmilat/utils"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/credential"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// Merging cash notes (I-26). cash_consolidate takes only pubkey-identified
// sources, and a cash note moves out of cash mode only by a cash_transfer that
// its secret authorizes. So a merge first moves each cash note, whole, onto a
// key made for that merge alone — no local identity needed — and then
// consolidates as usual, signing those sources with that key.
//
// The key lives on each moved note's own ledger entry (Entry.ThrowawayKey), so a
// moved note is an ordinary held bill that the key spends: nothing is stranded if
// the merge stops halfway, and the next `consolidate` finishes it. The key is
// written ahead of each move (PendingThrowawayKey) and promoted once the move is
// confirmed; a move whose answer never came is settled later by asking the Hub
// which of the two credentials it accepts (settlePendingMoves), never resent blind.

// newThrowawayKey returns a fresh private key and its public key, both hex.
func newThrowawayKey() (priv, pub string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	priv = hex.EncodeToString(b)
	pub, err = utils.GetPublicKey(priv)
	return priv, pub, err
}

// isCashNote reports a held entry that is a cash note this ledger can spend.
func isCashNote(e *ledger.Entry) bool {
	return e.ThrowawayKey == "" && e.IdentityRequired != nil && !*e.IdentityRequired && e.CashSecret != ""
}

// moveCash places one whole-amount cash_transfer of entry to target, spent with
// cred, as a call written ahead for purpose. On success the caller applies the
// result and closes the returned call before its Save.
func moveCash(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, cred nipcash.Credential, target nipcash.Target, purpose string) (*nipcash.CashTransferResult, *ledger.Call, error) {
	if entry.AmountMillis == nil {
		return nil, nil, output.RuntimeError(cmd, fmt.Errorf("%s has no known amount", entry.ID))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	caller, err := dialCallerFn(ctx, entry.Token)
	if err != nil {
		return nil, nil, classifyCashSpendErr(cmd, &notSentError{err}, transferVerb(purpose), entry.ID)
	}
	defer caller.Close()
	kind, pub, secret := authorizerOf(cmd, entry, cred)
	spec := callSpec{Purpose: purpose, SourceEntryIDs: []string{entry.ID}, AuthorizerKind: kind,
		AuthorizerPubkey: pub, AuthorizerSecret: secret, TargetCredential: entry.Token}
	return placeTransfer(ctx, cmd, l, caller, spec, nipcash.CashTransferParams{Credential: cred, To: target, CurrentAmount: *entry.AmountMillis})
}

// moveCashNotesToKey moves every cash note among ids onto the throwaway key
// priv/pub, saving after each. Returns the ids it moved. A definite refusal moves
// the already-moved ones back to fresh cash notes and returns the refusal; no
// answer leaves that note's move open (its key kept on the entry, looked up
// later) and returns the indeterminate error.
func moveCashNotesToKey(cmd *cobra.Command, l *ledger.Ledger, ids []string, priv, pub string) ([]string, error) {
	var moved []string
	for _, id := range ids {
		e, ok := l.Find(id)
		if !ok || !isCashNote(e) {
			continue
		}
		// The key is written ahead in the same save as the call: if the move
		// lands and the answer is lost, it is the only thing that spends the note.
		e.PendingThrowawayKey = priv
		res, call, err := moveCash(cmd, l, e, nipcash.BySecret(e.CashSecret), nipcash.Pubkey(pub), purposeMergeMove)
		e, _ = l.Find(id)
		if err != nil {
			if output.AsCLIError(err).Code == output.CodeIndeterminate {
				return moved, err
			}
			e.PendingThrowawayKey = ""
			_ = l.Save()
			if uErr := undoMoves(cmd, l, moved); uErr != nil {
				return nil, withRecovery(err, uErr)
			}
			return nil, err
		}
		followMove(e, res)
		promoteThrowaway(e)
		closeCall(l, call, ledger.CallDone, "")
		if err := l.Save(); err != nil {
			return moved, reportUnsavedResult(cmd, err, "Merge (moving a note)", fmt.Sprintf("%s is now spent by the private key %s", e.ID, priv))
		}
		moved = append(moved, id)
	}
	return moved, nil
}

// followMove points e at the wallet a whole-amount move left its value in. A
// move to a pubkey stays in place, but one to a cash secret does only on a
// wallet that has only ever had one recipient (NIP-CASH §Transferring and
// Splitting a Slice): a note that went onto a merge key and back has had two,
// so the Hub spins it off into a new wallet and tombstones the old slice.
func followMove(e *ledger.Entry, res *nipcash.CashTransferResult) {
	if res == nil || res.NewWalletToken == "" {
		return
	}
	e.Token = strings.ToLower(res.NewWalletToken)
	if res.NewWalletPubkey != "" {
		e.WalletPubkey = res.NewWalletPubkey
	}
	e.Verified, e.MinterPubkey = returnedBillCheck(e.Token, e.MinterPubkey)
}

// becomeCashNote records a confirmed re-key: secret, which only this ledger
// holds, now spends the bill as an ordinary cash note.
func becomeCashNote(e *ledger.Entry, secret string) {
	e.CashSecret = secret
	e.ThrowawayKey = ""
	e.IdentityRequired = ptrTo(false)
	e.CashProtection = ledger.CashProtected
}

// promoteThrowaway records a confirmed move: the key now spends the note.
func promoteThrowaway(e *ledger.Entry) {
	e.ThrowawayKey = e.PendingThrowawayKey
	e.PendingThrowawayKey = ""
	e.CashSecret = ""
	e.IdentityRequired = ptrTo(true)
}

// isDefiniteRefusal reports a Hub's explicit answer that it did not apply a
// request, or the SDK's refusal to send it — the failures after which nothing
// moved.
func isDefiniteRefusal(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) || isRejectedBeforeSend(err)
}

// undoMoves moves the notes of a refused merge back to fresh cash notes. Its
// error, when not nil, carries the recovery of a note moved but not saved.
func undoMoves(cmd *cobra.Command, l *ledger.Ledger, ids []string) error {
	converted, unsaved := toCashNotes(cmd, l, ids)
	if len(unsaved) > 0 {
		return unsavedNotesError(cmd, unsaved)
	}
	if len(converted) > 0 {
		jsonMode, _ := cmd.Flags().GetBool("json")
		output.Notef(jsonMode, "The merge was refused, so the notes it had moved are cash notes again.")
	}
	return nil
}

// withRecovery keeps classified as the error (its code and message still say why
// the operation failed) and attaches unsaved's recovery to it, so a note moved but
// not saved is never dropped for the reason it was moved.
func withRecovery(classified, unsaved error) error {
	var ce *output.CLIError
	if !errors.As(classified, &ce) {
		return unsaved
	}
	ce.Recovery = output.AsCLIError(unsaved).Recovery
	return classified
}

// errNoteNotSaved is the cause reportUnsavedResult names for toCashNotes's
// unsaved notes: the move landed on the Hub, the local save of it did not.
var errNoteNotSaved = errors.New("the local save failed")

// unsavedNotesError reports notes toCashNotes moved but could not save, with
// each one's <token>#<secret> as the recovery.
func unsavedNotesError(cmd *cobra.Command, unsaved []string) error {
	return reportUnsavedResult(cmd, errNoteNotSaved, "Turning a note back into a cash note",
		"Save this now, it will not be shown again: "+strings.Join(unsaved, " "))
}

// toCashNotes moves bills off their throwaway key onto fresh cash notes, each a
// call written ahead with its new secret, so whatever a merge leaves behind —
// notes it moved before a refusal, a split's change, an interim bill — is an
// ordinary cash note again. Best-effort: a bill that cannot be moved stays an
// ordinary held bill its key spends, reported as a merge left midway; one with
// a call still open is skipped even when named.
//
// Returns the ids it converted, and the <token>#<secret> of each note the Hub
// moved but whose save then failed: disk still names the bill's old state, so
// that string is the only way back to it once the process exits.
func toCashNotes(cmd *cobra.Command, l *ledger.Ledger, ids []string) (converted, unsaved []string) {
	for _, id := range ids {
		e, ok := l.Find(id)
		if !ok || e.ThrowawayKey == "" || e.Status != ledger.StatusHeld || e.PendingThrowawayKey != "" {
			continue
		}
		if _, open := l.OpenCallFor(id); open {
			continue
		}
		target, err := nipcash.NewCashTarget()
		if err != nil {
			continue
		}
		res, call, err := moveCash(cmd, l, e, nipcash.BySigning(e.ThrowawayKey), target, purposeUnmergeMove)
		if err != nil {
			continue
		}
		e, _ = l.Find(id)
		followMove(e, res)
		becomeCashNote(e, target.Secret())
		closeCall(l, call, ledger.CallDone, "")
		if err := l.Save(); err != nil {
			unsaved = append(unsaved, entryRecoveryString(e))
			continue
		}
		converted = append(converted, id)
	}
	return converted, unsaved
}

// leftoverKeyBills is every held bill still on a throwaway key with no move
// open: something a merge left behind, to be turned back into a cash note.
func leftoverKeyBills(l *ledger.Ledger) []string {
	var ids []string
	for _, e := range l.Entries {
		if e.Status != ledger.StatusHeld || e.ThrowawayKey == "" || e.PendingThrowawayKey != "" {
			continue
		}
		if _, open := l.OpenCallFor(e.ID); open {
			continue
		}
		ids = append(ids, e.ID)
	}
	return ids
}

// probeCredentialFn asks a bill's Hub whether cred still spends it — swapped out
// in tests. ok=false with err=nil means "it does not"; err means no answer.
var probeCredentialFn = probeCredential

func probeCredential(e *ledger.Entry, cred nipcash.Credential) (bool, error) {
	_, ok, err := probeAmount(e, cred)
	return ok, err
}

// probeAmountFn is probeCredential with the amount the bill answers for cred —
// swapped out in tests.
var probeAmountFn = probeAmount

// The identity asked about is the credential's own (nipcash.OwnRow): a row this
// credential does not own, a claimed or expired one, or a destroyed bill all
// answer "it does not".
func probeAmount(e *ledger.Entry, cred nipcash.Credential) (uint64, bool, error) {
	tok, err := nipcash.Decode(e.Token)
	if err != nil {
		return 0, false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := dialBill(ctx, e.Token)
	if err != nil {
		return 0, false, err
	}
	defer client.Close()
	r, err := client.CheckClaim(ctx, cred, tok)
	var gone *nipcash.BillGone
	switch {
	case err == nil:
		return r.Row.AmountMillis, r.Claimable, nil
	case errors.Is(err, nipcash.ErrClaimNotFound), errors.As(err, &gone), isDefiniteRefusal(err):
		return 0, false, nil
	}
	return 0, false, err
}

// settlePendingMoves finishes the record of a merge move whose answer never
// came: if the throwaway key now spends the note, the move happened; if its cash
// secret still does, it did not. Otherwise it is left for a later run. Never
// resends anything. Then turns whatever a merge left on a throwaway key back into
// cash notes: no merge is in progress when a command starts. Its error, when not
// nil, carries the recovery of a note moved but not saved.
func settlePendingMoves(cmd *cobra.Command, l *ledger.Ledger) error {
	changed := false
	for i := range l.Entries {
		e := &l.Entries[i]
		if e.Status != ledger.StatusHeld {
			continue
		}
		if e.PendingThrowawayKey != "" && settlePendingKey(e) {
			changed = true
			if e.PendingThrowawayKey == "" && e.ThrowawayKey != "" {
				// The key spends the note: its move happened.
				if c, open := l.OpenCallFor(e.ID); open && c.Purpose == purposeMergeMove {
					closeCall(l, c, ledger.CallDone, "settled from the bill's state")
				}
			}
		}
	}
	if changed {
		_ = l.Save()
	}
	if _, unsaved := toCashNotes(cmd, l, leftoverKeyBills(l)); len(unsaved) > 0 {
		return unsavedNotesError(cmd, unsaved)
	}
	return nil
}

// settlePendingKey resolves a move onto a throwaway key, reporting a change.
func settlePendingKey(e *ledger.Entry) bool {
	if _, err := utils.GetPublicKey(e.PendingThrowawayKey); err != nil {
		return false
	}
	if ok, err := probeCredentialFn(e, nipcash.BySigning(e.PendingThrowawayKey)); err == nil && ok {
		promoteThrowaway(e)
		return true
	}
	if ok, err := probeCredentialFn(e, nipcash.BySecret(e.CashSecret)); err == nil && ok {
		e.PendingThrowawayKey = ""
		return true
	}
	return false
}

// pendingMerge summarizes held notes moved for a merge that has not finished:
// how many, and the earliest deadline among them (nil when none is known).
func pendingMerge(l *ledger.Ledger) (count int, earliest *int64) {
	for _, e := range l.Held() {
		if e.ThrowawayKey == "" && e.PendingThrowawayKey == "" {
			continue
		}
		count++
		earliest = earliestExpiry(earliest, e.ExpiresAt)
	}
	return count, earliest
}

// pendingMergeJSON is pendingMerge for --json: null when nothing is mid-merge.
func pendingMergeJSON(l *ledger.Ledger) any {
	n, earliest := pendingMerge(l)
	if n == 0 {
		return nil
	}
	return map[string]any{"notes": n, "earliest_expires_at": earliest}
}

// printPendingMerge tells the holder about notes left on a throwaway key by a
// merge that has not finished, and how to finish it.
func printPendingMerge(l *ledger.Ledger) {
	n, earliest := pendingMerge(l)
	if n == 0 {
		return
	}
	when := ""
	if earliest != nil {
		when = ", the first expiring " + time.Unix(*earliest, 0).UTC().Format("2006-01-02 15:04 UTC")
	}
	output.Printf("\nMid-merge: %d note(s) moved for a merge that has not finished%s — `cashctl consolidate` finishes it.\n", n, when)
}

// sendFromEntry places one cash_transfer from a held bill as a call written
// ahead for purpose.
func sendFromEntry(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, params nipcash.CashTransferParams, purpose string) (*nipcash.CashTransferResult, *ledger.Call, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	caller, err := dialCallerFn(ctx, entry.Token)
	if err != nil {
		return nil, nil, classifyCashSpendErr(cmd, &notSentError{err}, "transfer", entry.ID)
	}
	defer caller.Close()
	kind, pub, secret := authorizerOf(cmd, entry, params.Credential)
	spec := callSpec{Purpose: purpose, SourceEntryIDs: []string{entry.ID}, AuthorizerKind: kind,
		AuthorizerPubkey: pub, AuthorizerSecret: secret, TargetCredential: entry.Token}
	return placeTransfer(ctx, cmd, l, caller, spec, params)
}

// mergeThenSend sends sendAmount to target out of sources merged under the
// throwaway key priv (I-26, the cash notes already moved onto it), or under the
// local identity pub when priv is empty — never through an unsaved interim bill
// (#96). An exact sum is one cash_consolidate straight to the recipient.
// Otherwise the merge is SAVED before the send, so a crash or lost reply between
// the two leaves a recoverable bill: on a refused send a key bill turns back into
// a cash note and an identity bill simply stays held; an unanswered one stays
// held for `cash status` (a key bill is settled by the next command).
func mergeThenSend(cmd *cobra.Command, l *ledger.Ledger, tokens []string, sources []nipcash.Source, sourceIDs []string,
	sum, sendAmount uint64, priv, pub, toValue string, target credential.ResolvedTarget) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	limit := max(hubConsolidateCapFn(tokens[0]), 2)

	if sum == sendAmount {
		var merged *ledger.Entry
		err := WithSpinner(jsonMode, "Transferring...", func() error {
			var cErr error
			merged, _, cErr = consolidateInRounds(cmd, l, tokens, sources, sourceIDs, target.Target, false, limit, priv)
			return cErr
		})
		if err != nil {
			return err
		}
		// The bill is the recipient's: a cash-note result is handed out, not held.
		if _, isCash := target.Target.(*nipcash.CashTarget); isCash {
			_ = l.SetStatus(merged.ID, ledger.StatusTransferred)
		}
		res := &nipcash.CashTransferResult{AmountMillis: sendAmount, NewWalletToken: merged.Token, NewWalletPubkey: merged.WalletPubkey}
		return printAndSaveTransferResult(cmd, l, res, nil, sendAmount, toValue, target, sourceIDs, ledger.Entry{}, merged.Token)
	}

	var merged *ledger.Entry
	err := WithSpinner(jsonMode, "Merging...", func() error {
		var cErr error
		merged, _, cErr = consolidateInRounds(cmd, l, tokens, sources, sourceIDs, nipcash.Pubkey(pub), true, limit, priv)
		return cErr
	})
	if err != nil {
		return err
	}
	merged.ThrowawayKey = priv
	mergedID := merged.ID
	if err := l.Save(); err != nil {
		hint := entryRecoveryHint(merged)
		if priv != "" {
			hint = fmt.Sprintf("%s — spent by the private key %s", hint, priv)
		}
		return reportUnsavedResult(cmd, err, "Transfer (merge)", hint)
	}
	// The merged bill is the key's, or the local identity's: never --as, which
	// names the sources' credential.
	cred := nipcash.BySigning(priv)
	if priv == "" {
		var cErr error
		if cred, cErr = localCashCredential(cmd); cErr != nil {
			return output.RuntimeError(cmd, cErr)
		}
	}

	merged, _ = l.Find(mergedID)
	var result *nipcash.CashTransferResult
	var call *ledger.Call
	var sendErr error
	_ = WithSpinner(jsonMode, "Transferring...", func() error {
		result, call, sendErr = sendFromEntry(cmd, l, merged, nipcash.CashTransferParams{
			Credential: cred, To: target.Target,
			CurrentAmount: *merged.AmountMillis, SplitAmount: &sendAmount,
		}, purposeSplit)
		return nil
	})
	if sendErr != nil {
		if priv != "" && output.AsCLIError(sendErr).Code != output.CodeIndeterminate {
			// Refused, or never sent: the merged bill is still whole and ours.
			if _, unsaved := toCashNotes(cmd, l, []string{mergedID}); len(unsaved) > 0 {
				return withRecovery(sendErr, unsavedNotesError(cmd, unsaved))
			}
		}
		return sendErr
	}
	_ = l.SetStatus(mergedID, ledger.StatusTransferred)
	merged, _ = l.Find(mergedID)
	// consolidated_from names the bills the caller held, not the interim merge.
	return printAndSaveTransferResult(cmd, l, result, call, sendAmount, toValue, target, sourceIDs,
		ledger.Entry{IdentityRequired: ptrTo(true), MinterPubkey: merged.MinterPubkey, ThrowawayKey: priv}, merged.Token)
}
