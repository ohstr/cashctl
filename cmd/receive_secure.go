package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// protectedStatus is receive's own abstracted report of what the automatic
// protect step did — never named "transfer"/"consolidate"/"bearer_secret"
// to the user, only the outcome: rekeyed | consolidated | not_applicable |
// declined | failed. Surfaced under --json alongside "entry".
type protectedStatus struct {
	Status           string
	ConsolidatedWith []string
	FinalEntryID     string
	Error            string
	// LikelyWrongSecret is set when Status is "failed" and the decline
	// looks like the embedded bearer_secret itself was wrong (NWC code
	// NOT_FOUND), not a transient network/server issue — unlike an
	// ordinary failed-protect case, this likely means the entry was
	// never really spendable at all.
	LikelyWrongSecret bool
	// PendingSecretUnresolved is set when Status is "failed" and the
	// failure was a transport-level one (timeout, dropped connection) with
	// no definitive answer from the Hub — the rekey may have landed
	// anyway. The candidate secret is safely on disk regardless (see
	// protectRekeyOnly's own doc comment) and resolveCredential tries it
	// automatically on the next spend of this entry — this is never a
	// dead end the way LikelyWrongSecret's case is.
	PendingSecretUnresolved bool
}

func (s protectedStatus) json() map[string]any {
	out := map[string]any{"status": s.Status}
	if len(s.ConsolidatedWith) > 0 {
		out["consolidated_with"] = s.ConsolidatedWith
	}
	if s.FinalEntryID != "" {
		out["final_entry_id"] = s.FinalEntryID
	}
	if s.Error != "" {
		out["error"] = s.Error
	}
	if s.LikelyWrongSecret {
		out["likely_wrong_secret"] = true
	}
	if s.PendingSecretUnresolved {
		out["pending_secret_unresolved"] = true
	}
	return out
}

// isWrongSecretDecline reports whether err looks like a bearer_secret
// mismatch (NWC code NOT_FOUND) rather than some other failure — NIP-CASH
// §Redemption Metadata guarantees a wrong secret always declines this way.
func isWrongSecretDecline(err error) bool {
	var walletErr *relayclient.WalletError
	return errors.As(err, &walletErr) && walletErr.Code == "NOT_FOUND"
}

// protectBearerReceipt is receive's automatic follow-up for a freshly
// received bearer-mode entry: confirm with the user, then re-key it
// (nipcashclient.RekeyBearerSlice) — optionally consolidating with other
// held same-minter tokens — so the original, possibly-shared secret can
// no longer spend it. Best-effort: a failure here never fails receive
// itself, since the bill is already genuinely theirs by the time this
// runs (checkClaimWithCashHub already passed). Returns the report to
// surface and the entry that should now be treated as "the" result of
// this receive (the same entry for rekeyed/declined/failed/not_applicable,
// a new one for consolidated).
func protectBearerReceipt(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, isBearer bool) (protectedStatus, *ledger.Entry) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if !isBearer {
		return protectedStatus{Status: "not_applicable"}, entry
	}
	// defaultYes=true is intentional here, unlike a money-moving prompt:
	// declining leaves the holding MORE exposed (still shared), not less —
	// an unattended --yes/--json/EOF receive protecting by default is the
	// safe direction, same reasoning as auto-securing at all. What the
	// audit that found this actually flagged: the prompt itself never
	// explained the mechanism, so even the interactive path left a human
	// agreeing to something opaque — fixed by naming it plainly, not by
	// flipping the default.
	if !Confirm(cmd, true, "This cash is still shared — anyone holding it can spend it.\n"+
		"Protect it now? (re-keys it; may also merge holdings from the same issuer)") {
		return protectedStatus{Status: "declined"}, entry
	}

	var consolidateWith []nipcash.Source
	var consolidateWithIDs []string
	err := WithSpinner(jsonMode, "Checking holdings...", func() error {
		cw, ids, cErr := buildConsolidateWith(cmd, l, entry)
		consolidateWith, consolidateWithIDs = cw, ids
		return cErr
	})
	if err != nil {
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}

	// The no-merge case gets its own, write-ahead-safe path (see its own
	// doc comment on why) rather than nipcashclient.RekeyBearerSlice.
	// The merge case still needs RekeyBearerSlice's own multi-step
	// composite (an interim reassignment onto a pubkey identity, THEN a
	// consolidate) — its own fresh secret is generated deep inside that
	// call, not accessible to persist ahead of time without reimplementing
	// the composite here, which would risk the exact class of interop bug
	// this same call already has server-side (see docs/private's audit of
	// this Hub's "decrypt delivery" failures on this path).
	if len(consolidateWith) == 0 {
		return protectRekeyOnly(cmd, l, entry, jsonMode)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	myPubHex, err := localPubKeyHex(cmd)
	if err != nil {
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}
	cred, err := localCashCredential(cmd)
	if err != nil {
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}
	params := nipcashclient.RekeyBearerSliceParams{
		BearerSlice: nipcash.Source{
			WalletPubkey: entry.WalletPubkey,
			Amount:       *entry.AmountMillis,
			Credential:   nipcash.BySecret(entry.BearerSecret),
		},
		MintSignature:     entry.MinterPubkey != nil,
		InterimIdentity:   nipcash.Pubkey(myPubHex),
		InterimCredential: cred,
		ConsolidateWith:   consolidateWith,
	}

	var dialErr bool
	var result *nipcashclient.RekeyBearerSliceResult
	err = WithSpinner(jsonMode, "Protecting...", func() error {
		client, cErr := nipcashclient.Connect(ctx, entry.Token)
		if cErr != nil {
			dialErr = true
			return cErr
		}
		defer client.Close()
		r, cErr := client.RekeyBearerSlice(ctx, params)
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		if dialErr {
			printProtectFailure(jsonMode, err)
			return protectedStatus{Status: "failed", Error: err.Error()}, entry
		}
		var partial *nipcashclient.PartialProgressError
		if errors.As(err, &partial) && partial.Transferred != nil {
			// The interim reassignment landed for real — the old bearer
			// secret is already dead, even though consolidation itself
			// didn't finish. Apply that real effect before reporting the
			// failure, per the save-immediately rule: never leave the
			// ledger mismatched with the last wire call that actually
			// succeeded. A failure of THIS save is the harder case: the
			// Hub-side effect is real and, unlike the ordinary path below,
			// there is no pending-secret trail to fall back on for a
			// reassign-to-pubkey (not bearer) interim step — report it
			// plainly rather than pretending Save() always works.
			entry.BearerSecret = ""
			entry.IdentityRequired = ptrTo(true)
			// No longer bearer-mode at all — reassigned to the local
			// identity's own pubkey — so "shared vs protected" no longer
			// applies (see Entry.BearerProtection's own doc comment: empty
			// means n/a, the same as any other pubkey-mode entry).
			entry.BearerProtection = ""
			// The interim's own follow-up consolidate merges entry
			// alongside consolidateWithIDs — if ITS failure is the same
			// decrypt-family ambiguity cash_consolidate.go's own direct
			// path can hit (isAmbiguousDeliveryErr's doc comment), those
			// other, already-held sources may be consumed too. Reconcile
			// the same way that path does: ask each source's own token,
			// independently, whether the Hub still has a claim for it.
			reportErr := err
			if isAmbiguousDeliveryErr(err) {
				if gone := reconcileAmbiguousSources(cmd, l, consolidateWithIDs); len(gone) > 0 {
					reportErr = fmt.Errorf("%w (confirmed consumed on the Hub despite the unreadable reply: %s — marked accordingly so they won't be offered again, though the merged result itself could not be recovered)",
						err, strings.Join(gone, ", "))
				} else {
					reportErr = warnAmbiguousDelivery(err, consolidateWithIDs)
				}
			}
			if saveErr := l.Save(); saveErr != nil {
				reportErr = fmt.Errorf("%w (and saving that locally also failed: %v — the ledger may still show the old, now-dead secret)", reportErr, saveErr)
				printProtectPartialFailure(jsonMode, reportErr)
				return protectedStatus{Status: "failed", Error: reportErr.Error()}, entry
			}
			printProtectPartialFailure(jsonMode, reportErr)
			return protectedStatus{Status: "failed", Error: reportErr.Error()}, entry
		}
		wrongSecret := isWrongSecretDecline(err)
		if wrongSecret {
			printProtectFailureWrongSecret(jsonMode, err)
		} else {
			printProtectFailure(jsonMode, err)
		}
		return protectedStatus{Status: "failed", Error: err.Error(), LikelyWrongSecret: wrongSecret}, entry
	}

	for _, id := range consolidateWithIDs {
		_ = l.SetStatus(id, ledger.StatusConsolidated)
	}
	_ = l.SetStatus(entry.ID, ledger.StatusConsolidated)
	var newMinterPubkey *string
	if entry.MinterPubkey != nil {
		if newTok, decErr := nipcash.Decode(result.NewToken); decErr == nil {
			newMinterPubkey = minterPubkeyFromToken(newTok)
		}
	}
	newEntry, addErr := l.Add(ledger.Entry{
		Token:            result.NewToken,
		WalletPubkey:     result.NewWalletPubkey,
		BearerSecret:     result.NewSecret,
		IdentityRequired: ptrTo(false),
		AmountMillis:     &result.AmountMillis,
		Verified:         true,
		MinterPubkey:     newMinterPubkey,
		BearerProtection: ledger.BearerProtected,
	})
	if addErr != nil {
		printProtectFailure(jsonMode, addErr)
		return protectedStatus{Status: "failed", Error: addErr.Error()}, entry
	}
	l.AppendHistory("secure", fmt.Sprintf("combined with %d other holding(s) from the same issuer into one %s note", len(consolidateWithIDs), output.FormatAmount(int64(result.AmountMillis))))
	if err := l.Save(); err != nil {
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}
	if !jsonMode {
		fmt.Printf("Protected and merged %d holding(s) into %s.\n", len(consolidateWithIDs), output.FormatAmount(int64(result.AmountMillis)))
	}
	return protectedStatus{Status: "consolidated", ConsolidatedWith: consolidateWithIDs, FinalEntryID: newEntry.ID}, newEntry
}

// protectRekeyOnly is protectBearerReceipt's no-merge path: re-key entry's
// bearer secret in place, without going through nipcashclient.RekeyBearerSlice.
//
// A bearer target's secret is generated purely locally — NIP-CASH §Bearer
// Slices: the caller supplies only a one-way commitment over the wire, the
// secret itself never crosses it — so there is no reason to let placing the
// call be the difference between "the new secret exists on disk" and "the
// new secret exists only in this process's own memory, gone the instant it
// dies." nipcash.NewBearerTarget() is generated here, in cashctl's own
// code, and entry.PendingBearerSecret is saved BEFORE the call — the same
// write-ahead discipline a database uses for its own log.
//
// Once persisted, a kill or lost response can leave the outcome genuinely
// ambiguous: NIP-CASH has no read-only way to ask the Hub which of two
// candidate secrets it accepted (nipcashclient.CheckClaim's own doc
// comment — a bearer match only proves *some* recipient exists, never
// *which* secret). resolveCredential's own fallback resolves that
// ambiguity the only way the protocol allows: by trying the pending secret
// on the entry's next real spend attempt if the usual one is declined.
func protectRekeyOnly(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, jsonMode bool) (protectedStatus, *ledger.Entry) {
	bt := nipcash.NewBearerTarget()
	entry.PendingBearerSecret = bt.Secret()
	if err := l.Save(); err != nil {
		// Nothing has been sent to the Hub yet — entry.BearerSecret is
		// still the only real secret, so this is an ordinary failure.
		entry.PendingBearerSecret = ""
		printProtectFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error()}, entry
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var dialErr bool
	var result *nipcash.CashTransferResult
	err := WithSpinner(jsonMode, "Protecting...", func() error {
		client, cErr := nipcashclient.Connect(ctx, entry.Token)
		if cErr != nil {
			dialErr = true
			return cErr
		}
		defer client.Close()
		r, cErr := client.CashTransfer(ctx, nipcash.CashTransferParams{
			Credential:    nipcash.BySecret(entry.BearerSecret),
			To:            bt,
			CurrentAmount: *entry.AmountMillis,
			MintSignature: entry.MinterPubkey != nil,
		})
		if cErr != nil {
			return cErr
		}
		result = r
		return nil
	})
	if err != nil {
		var walletErr *relayclient.WalletError
		// A dial failure never reached the Hub at all; a *WalletError is
		// the Hub actually answering with a real decline. Either way, the
		// call is DEFINITELY known not to have taken effect — safe to
		// retract the pending secret and keep reporting the old one.
		if dialErr || errors.As(err, &walletErr) {
			entry.PendingBearerSecret = ""
			_ = l.Save() // best-effort tidy-up; entry.BearerSecret itself is unchanged either way
			wrongSecret := isWrongSecretDecline(err)
			if wrongSecret {
				printProtectFailureWrongSecret(jsonMode, err)
			} else {
				printProtectFailure(jsonMode, err)
			}
			return protectedStatus{Status: "failed", Error: err.Error(), LikelyWrongSecret: wrongSecret}, entry
		}
		// Ambiguous: a transport-level failure (timeout, dropped
		// connection) with no definitive answer from the Hub. Leave
		// PendingBearerSecret exactly as already saved above — don't
		// touch it either way — so resolveCredential can try it on the
		// next real spend of this entry.
		printProtectAmbiguousFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error(), PendingSecretUnresolved: true}, entry
	}
	_ = result // the Hub never returns the secret itself for this call — see CashTransferResult's own doc comment; bt.Secret() IS the new secret

	entry.BearerSecret = bt.Secret()
	entry.PendingBearerSecret = ""
	entry.BearerProtection = ledger.BearerProtected
	l.AppendHistory("secure", "re-keyed this cash so the shared secret can no longer spend it")
	if err := l.Save(); err != nil {
		// The rekey is CONFIRMED (the call above returned no error) — but
		// this save's own failure is harmless data-safety-wise: SQLite
		// commits l.save()'s whole batch atomically (internal/ledger.Save's
		// own doc comment), so a failed commit here leaves the row exactly
		// as the write-ahead save above left it — BearerSecret still the
		// OLD value, PendingBearerSecret still bt.Secret() — which is
		// exactly the state resolveCredential's fallback already knows how
		// to recover from. Report it the same way as the ambiguous case
		// above rather than inventing a third message for what is, from
		// disk's point of view, the identical situation.
		printProtectAmbiguousFailure(jsonMode, err)
		return protectedStatus{Status: "failed", Error: err.Error(), PendingSecretUnresolved: true}, entry
	}
	if !jsonMode {
		fmt.Println("Protected.")
	}
	return protectedStatus{Status: "rekeyed"}, entry
}

// buildConsolidateWith finds other held, consolidation-eligible entries
// sharing entry's own MinterPubkey — cashctl's own client-side "same
// issuer" signal (see internal/ledger/cashselect.go's own reasoning,
// already used for transfer's auto-consolidate step). No MinterPubkey at
// all (no verified mint signature) means no reliable way to group at
// all, so it always returns empty in that case, never a false positive.
func buildConsolidateWith(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry) ([]nipcash.Source, []string, error) {
	if entry.MinterPubkey == nil {
		return nil, nil, nil
	}
	groups := ledger.GroupByMinter(ledger.GroupableForConsolidation(l.Held()))
	group := groups[*entry.MinterPubkey]
	var sources []nipcash.Source
	var ids []string
	for i := range group {
		e := &group[i]
		if e.ID == entry.ID {
			continue
		}
		src, err := sourceFromEntry(cmd, l, e)
		if err != nil {
			return nil, nil, err
		}
		sources = append(sources, src)
		ids = append(ids, e.ID)
	}
	return sources, ids, nil
}

func printProtectFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received, but protecting failed (%v) — still shared. Retry: `cashctl consolidate --to cash`.\n", err)
}

// printProtectAmbiguousFailure is printProtectFailure's counterpart for a
// transport-level failure with no definitive answer from the Hub — unlike
// the ordinary case, this is never a dead end: the candidate secret this
// attempt generated is already safely on disk, and the next real spend of
// this entry (redeem/transfer/consolidate) tries it automatically if the
// usual secret is declined (see resolveCredential's own doc comment).
func printProtectAmbiguousFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received, but couldn't confirm protecting worked (%v).\nMay have worked anyway — no action needed, your next spend tries both.\n", err)
}

// printProtectFailureWrongSecret is printProtectFailure's counterpart for
// isWrongSecretDecline — printed instead of, never alongside, the
// ordinary message, whose "still shared" framing is false reassurance
// when the real problem is that this cash was never genuinely redeemable
// with the secret presented at all.
func printProtectFailureWrongSecret(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received, but the secret doesn't match (%v) — likely wrong/truncated. Ask for the full \"token#secret\" string again.\n", err)
}

func printProtectPartialFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received — old secret is dead, merge failed partway (%v). Finish: `cashctl consolidate --to cash`.\n", err)
}
