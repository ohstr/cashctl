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

// securedStatus is receive's own abstracted report of what the automatic
// securing step did — never named "transfer"/"consolidate"/"bearer_secret"
// to the user, only the outcome: rekeyed | consolidated | not_applicable |
// declined | failed. Surfaced under --json alongside "entry".
type securedStatus struct {
	Status           string
	ConsolidatedWith []string
	FinalEntryID     string
	Error            string
	// LikelyWrongSecret is set when Status is "failed" and the decline
	// looks like the embedded bearer_secret itself was wrong (NWC code
	// NOT_FOUND), not a transient network/server issue — unlike an
	// ordinary failed-securing case, this likely means the entry was
	// never really spendable at all.
	LikelyWrongSecret bool
}

func (s securedStatus) json() map[string]any {
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

// secureBearerReceipt is receive's automatic follow-up for a freshly
// received bearer-mode entry: confirm with the user, then re-key it
// (nipcashclient.RekeyBearerSlice) — optionally consolidating with other
// held same-minter tokens — so the original, possibly-shared secret can
// no longer spend it. Best-effort: a failure here never fails receive
// itself, since the bill is already genuinely theirs by the time this
// runs (checkClaimWithCashHub already passed). Returns the report to
// surface and the entry that should now be treated as "the" result of
// this receive (the same entry for rekeyed/declined/failed/not_applicable,
// a new one for consolidated).
func secureBearerReceipt(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry, isBearer bool) (securedStatus, *ledger.Entry) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	if !isBearer {
		return securedStatus{Status: "not_applicable"}, entry
	}
	if !Confirm(cmd, true, "This cash was shared as a bearer note — anyone who saw the same code can still spend it too. Secure it now so only you can?") {
		return securedStatus{Status: "declined"}, entry
	}

	consolidateWith, consolidateWithIDs, err := buildConsolidateWith(cmd, l, entry)
	if err != nil {
		printSecureFailure(jsonMode, err)
		return securedStatus{Status: "failed", Error: err.Error()}, entry
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, entry.Token)
	if err != nil {
		printSecureFailure(jsonMode, err)
		return securedStatus{Status: "failed", Error: err.Error()}, entry
	}
	defer client.Close()

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
			printSecureFailure(jsonMode, err)
			return securedStatus{Status: "failed", Error: err.Error()}, entry
		}
		cred, err := localCashCredential(cmd)
		if err != nil {
			printSecureFailure(jsonMode, err)
			return securedStatus{Status: "failed", Error: err.Error()}, entry
		}
		params.InterimIdentity = nipcash.Pubkey(myPubHex)
		params.InterimCredential = cred
		params.ConsolidateWith = consolidateWith
	}

	result, err := client.RekeyBearerSlice(ctx, params)
	if err != nil {
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
			printSecurePartialFailure(jsonMode, err)
			return securedStatus{Status: "failed", Error: err.Error()}, entry
		}
		wrongSecret := isWrongSecretDecline(err)
		if wrongSecret {
			printSecureFailureWrongSecret(jsonMode, err)
		} else {
			printSecureFailure(jsonMode, err)
		}
		return securedStatus{Status: "failed", Error: err.Error(), LikelyWrongSecret: wrongSecret}, entry
	}

	if result.NewToken == "" {
		entry.BearerSecret = result.NewSecret
		l.AppendHistory("secure", "re-keyed this cash so the shared code can no longer spend it")
		if err := l.Save(); err != nil {
			printSecureFailure(jsonMode, err)
			return securedStatus{Status: "failed", Error: err.Error()}, entry
		}
		if !jsonMode {
			fmt.Println("Secured — re-keyed under a new code only you know.")
		}
		return securedStatus{Status: "rekeyed"}, entry
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
		printSecureFailure(jsonMode, addErr)
		return securedStatus{Status: "failed", Error: addErr.Error()}, entry
	}
	l.AppendHistory("secure", fmt.Sprintf("combined with %d other holding(s) from the same issuer into one %d %s note", len(consolidateWithIDs), result.AmountMillis, output.CurrencyUnit))
	if err := l.Save(); err != nil {
		printSecureFailure(jsonMode, err)
		return securedStatus{Status: "failed", Error: err.Error()}, entry
	}
	if !jsonMode {
		fmt.Printf("Secured and combined with %d other holding(s) from the same issuer — now one %d %s note only you can spend.\n", len(consolidateWithIDs), result.AmountMillis, output.CurrencyUnit)
	}
	return securedStatus{Status: "consolidated", ConsolidatedWith: consolidateWithIDs, FinalEntryID: newEntry.ID}, newEntry
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

func printSecureFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received, but automatic securing failed (%v) — this cash is still\nyours, but still spendable by anyone who has the same code you got it\nfrom. See `cashctl wallet show`, then `cashctl consolidate --to\nbearer-target` to secure it yourself later.\n", err)
}

// printSecureFailureWrongSecret is printSecureFailure's counterpart for
// isWrongSecretDecline — printed instead of, never alongside, the
// ordinary message, whose "still yours, still shared" framing is false
// reassurance when the real problem is that this cash was never
// genuinely redeemable with the secret presented at all.
func printSecureFailureWrongSecret(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received, but the automatic securing step was rejected as if the\nembedded secret doesn't actually match this cash (%v). This usually\nmeans the code you were given is wrong, truncated, or never valid —\nnot that this cash is merely \"still shared.\" Before assuming it's\nspendable, try `cashctl redeem` or `cashctl transfer` on it for real;\nif that also fails, whoever sent it should resend the exact, complete\ncombined string (token#secret).\n", err)
}

func printSecurePartialFailure(jsonMode bool, err error) {
	if jsonMode {
		return
	}
	fmt.Printf("Received, and the old code is already dead — but combining it with\nyour other holding failed partway through (%v). See `cashctl wallet\nshow`, then `cashctl consolidate --to bearer-target` to finish securing\nit.\n", err)
}
