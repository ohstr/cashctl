package cmd

import (
	"context"
	"errors"
	"fmt"
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
	if !Confirm(cmd, true, "Shared as bearer — still spendable by whoever has the code. Protect it now?") {
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

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	params := nipcashclient.RekeyBearerSliceParams{
		BearerSlice: nipcash.Source{
			WalletPubkey: entry.WalletPubkey,
			Amount:       *entry.AmountMillis,
			Credential:   nipcash.BySecret(entry.BearerSecret),
		},
		MintSignature: entry.MinterPubkey != nil,
	}
	if len(consolidateWith) > 0 {
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
		params.InterimIdentity = nipcash.Pubkey(myPubHex)
		params.InterimCredential = cred
		params.ConsolidateWith = consolidateWith
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
			// succeeded.
			entry.BearerSecret = ""
			entry.IdentityRequired = ptrTo(true)
			_ = l.Save()
			printProtectPartialFailure(jsonMode, err)
			return protectedStatus{Status: "failed", Error: err.Error()}, entry
		}
		wrongSecret := isWrongSecretDecline(err)
		if wrongSecret {
			printProtectFailureWrongSecret(jsonMode, err)
		} else {
			printProtectFailure(jsonMode, err)
		}
		return protectedStatus{Status: "failed", Error: err.Error(), LikelyWrongSecret: wrongSecret}, entry
	}

	if result.NewToken == "" {
		entry.BearerSecret = result.NewSecret
		l.AppendHistory("secure", "re-keyed this cash so the shared code can no longer spend it")
		if err := l.Save(); err != nil {
			printProtectFailure(jsonMode, err)
			return protectedStatus{Status: "failed", Error: err.Error()}, entry
		}
		if !jsonMode {
			fmt.Println("Protected.")
		}
		return protectedStatus{Status: "rekeyed"}, entry
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
	fmt.Printf("Received, but protecting failed (%v) — still shared. Retry: `cashctl consolidate --to bearer-target`.\n", err)
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
	fmt.Printf("Received — old code is dead, merge failed partway (%v). Finish: `cashctl consolidate --to bearer-target`.\n", err)
}
