package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
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

// moveCashFn places one whole-amount cash_transfer of entry to target, spent with
// cred — swapped out in tests.
var moveCashFn = moveCash

func moveCash(entry *ledger.Entry, cred nipcash.Credential, target nipcash.Target) (*nipcash.CashTransferResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, entry.Token)
	if err != nil {
		return nil, &notSentError{err}
	}
	defer client.Close()
	if entry.AmountMillis == nil {
		return nil, &notSentError{fmt.Errorf("%s has no known amount", entry.ID)}
	}
	return client.CashTransfer(ctx, nipcash.CashTransferParams{Credential: cred, To: target, CurrentAmount: *entry.AmountMillis})
}

// moveCashNotesToKey moves every cash note among ids onto the throwaway key
// priv/pub, saving after each. Returns the ids it moved. A definite refusal moves
// the already-moved ones back to fresh cash notes and returns the refusal; no
// answer leaves that note's key pending (settled on a later run) and returns an
// indeterminate error.
func moveCashNotesToKey(cmd *cobra.Command, l *ledger.Ledger, ids []string, priv, pub string) ([]string, error) {
	var moved []string
	for _, id := range ids {
		e, ok := l.Find(id)
		if !ok || !isCashNote(e) {
			continue
		}
		e.PendingThrowawayKey = priv
		if err := l.Save(); err != nil {
			e.PendingThrowawayKey = ""
			return moved, output.RuntimeError(cmd, err)
		}
		_, err := moveCashFn(e, nipcash.BySecret(e.CashSecret), nipcash.Pubkey(pub))
		if err != nil {
			var notSent *notSentError
			if errors.As(err, &notSent) || isDefiniteRefusal(err) {
				e.PendingThrowawayKey = ""
				_ = l.Save()
				undoMoves(cmd, l, moved)
				return nil, classifyCashSpendErr(cmd, err, "merge", id)
			}
			// No answer: the move may have happened. Keep the pending key.
			return moved, classifyCashSpendErr(cmd, err, "merge", id)
		}
		promoteThrowaway(e)
		if err := l.Save(); err != nil {
			return moved, reportUnsavedResult(cmd, err, "Merge (moving a note)", fmt.Sprintf("%s is now spent by the private key %s", e.ID, priv))
		}
		moved = append(moved, id)
	}
	return moved, nil
}

// promoteThrowaway records a confirmed move: the key now spends the note.
func promoteThrowaway(e *ledger.Entry) {
	e.ThrowawayKey = e.PendingThrowawayKey
	e.PendingThrowawayKey = ""
	e.CashSecret = ""
	e.IdentityRequired = ptrTo(true)
}

// isDefiniteRefusal reports a Hub's explicit answer that it did not apply a
// request — the one failure after which nothing moved.
func isDefiniteRefusal(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr)
}

// undoMoves moves the notes of a refused merge back to fresh cash notes.
func undoMoves(cmd *cobra.Command, l *ledger.Ledger, ids []string) {
	if toCashNotes(l, ids) > 0 {
		jsonMode, _ := cmd.Flags().GetBool("json")
		output.Notef(jsonMode, "The merge was refused, so the notes it had moved are cash notes again.")
	}
}

// toCashNotes moves bills off their throwaway key onto fresh cash notes, each
// secret written ahead of its move (PendingCashSecret), so whatever a merge leaves
// behind — notes it moved before a refusal, a split's change, an interim bill — is
// an ordinary cash note again. Best-effort: a bill that cannot be moved stays an
// ordinary held bill its key spends, reported as a merge left midway. Returns how
// many it converted.
func toCashNotes(l *ledger.Ledger, ids []string) int {
	converted := 0
	for _, id := range ids {
		e, ok := l.Find(id)
		if !ok || e.ThrowawayKey == "" || e.Status != ledger.StatusHeld {
			continue
		}
		target := nipcash.NewCashTarget()
		e.PendingCashSecret = target.Secret()
		if err := l.Save(); err != nil {
			e.PendingCashSecret = ""
			continue
		}
		if _, err := moveCashFn(e, nipcash.BySigning(e.ThrowawayKey), target); err != nil {
			if isDefiniteRefusal(err) {
				e.PendingCashSecret = ""
				_ = l.Save()
			}
			continue
		}
		e.CashSecret, e.PendingCashSecret = target.Secret(), ""
		e.ThrowawayKey = ""
		e.IdentityRequired = ptrTo(false)
		e.CashProtection = ledger.CashProtected
		_ = l.Save()
		converted++
	}
	return converted
}

// leftoverKeyBills is every held bill still on a throwaway key with no move
// pending: something a merge left behind, to be turned back into a cash note.
func leftoverKeyBills(l *ledger.Ledger) []string {
	var ids []string
	for _, e := range l.Entries {
		if e.Status == ledger.StatusHeld && e.ThrowawayKey != "" && e.PendingThrowawayKey == "" {
			ids = append(ids, e.ID)
		}
	}
	return ids
}

// probeCredentialFn asks a bill's Hub whether cred still spends it — swapped out
// in tests. ok=false with err=nil means "it does not"; err means no answer.
var probeCredentialFn = probeCredential

func probeCredential(e *ledger.Entry, cred nipcash.Credential, asPubkeyHex string) (bool, error) {
	tok, err := nipcash.Decode(e.Token)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, e.Token)
	if err != nil {
		return false, err
	}
	defer client.Close()
	_, err = client.CheckClaim(ctx, cred, tok, asPubkeyHex)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, nipcash.ErrClaimNotFound), isDefiniteRefusal(err):
		return false, nil
	}
	return false, err
}

// settlePendingMoves finishes the record of any move whose answer never came: if
// the throwaway key now spends the note, the move happened; if its cash secret
// still does, it did not. Otherwise it is left pending for a later run. Never
// resends the move. Then turns whatever a merge left on a throwaway key back into
// cash notes: no merge is in progress when a command starts.
func settlePendingMoves(l *ledger.Ledger) {
	changed := false
	for i := range l.Entries {
		e := &l.Entries[i]
		if e.PendingThrowawayKey == "" || e.Status != ledger.StatusHeld {
			continue
		}
		pub, err := utils.GetPublicKey(e.PendingThrowawayKey)
		if err != nil {
			continue
		}
		if ok, err := probeCredentialFn(e, nipcash.BySigning(e.PendingThrowawayKey), pub); err == nil && ok {
			promoteThrowaway(e)
			changed = true
			continue
		}
		if ok, err := probeCredentialFn(e, nipcash.BySecret(e.CashSecret), nipcash.NoLocalIdentity); err == nil && ok {
			e.PendingThrowawayKey = ""
			changed = true
		}
	}
	if changed {
		_ = l.Save()
	}
	toCashNotes(l, leftoverKeyBills(l))
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

// sendFromEntryFn places one cash_transfer from a held bill — swapped out in tests.
var sendFromEntryFn = sendFromEntry

func sendFromEntry(entry *ledger.Entry, params nipcash.CashTransferParams) (*nipcash.CashTransferResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, entry.Token)
	if err != nil {
		return nil, &notSentError{err}
	}
	defer client.Close()
	return client.CashTransfer(ctx, params)
}

// mergeThenSend sends sendAmount to target out of sources whose cash notes are
// already on the throwaway key priv (I-26), never through an unsaved interim bill.
// An exact sum is one cash_consolidate straight to the recipient. Otherwise the
// merge goes to the key and is SAVED before the send, so a crash or lost reply
// between the two leaves a recoverable bill: a refused send turns it back into a
// cash note, an unanswered one leaves it listed as a merge left midway (settled
// by the next command).
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
		return printAndSaveTransferResult(cmd, l, res, sendAmount, toValue, target, sourceIDs, ledger.Entry{}, merged.Token)
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
		return reportUnsavedResult(cmd, err, "Transfer (merge)",
			fmt.Sprintf("%s — spent by the private key %s", entryRecoveryHint(merged), priv))
	}

	parked, err := parkDestinationCashSecret(l, []string{mergedID}, target.Target)
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	merged, _ = l.Find(mergedID)
	var result *nipcash.CashTransferResult
	var sendErr error
	_ = WithSpinner(jsonMode, "Transferring...", func() error {
		result, sendErr = sendFromEntryFn(merged, nipcash.CashTransferParams{
			Credential: nipcash.BySigning(priv), To: target.Target,
			CurrentAmount: *merged.AmountMillis, SplitAmount: &sendAmount,
		})
		return nil
	})
	if sendErr != nil {
		classified := classifyCashSpendErr(cmd, sendErr, "transfer", mergedID)
		if output.AsCLIError(classified).Code != output.CodeIndeterminate {
			// Refused, or never sent: the merged bill is still whole and ours.
			toCashNotes(l, []string{mergedID})
		}
		return classified
	}
	_ = l.SetStatus(mergedID, ledger.StatusTransferred)
	parked.release()
	return printAndSaveTransferResult(cmd, l, result, sendAmount, toValue, target, []string{mergedID},
		ledger.Entry{IdentityRequired: ptrTo(true), MinterPubkey: merged.MinterPubkey, ThrowawayKey: priv}, merged.Token)
}
