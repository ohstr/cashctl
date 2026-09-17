package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/dial"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

func newCashReceiveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "receive <token>",
		Short: `"Cash-in" a token: verify and add it to your wallet`,
		Long:  `Verifies a cash token against the Hub before saving it. A bearer token needs its secret embedded as "<token>#<secret>".`,
		Example: `  cashctl receive lokicash1...
  cashctl receive lokicash1...#deadbeef`,
		Args: output.ExactArgs(1),
		RunE: runCashReceive,
	}
	return cmd
}

func runCashReceive(cmd *cobra.Command, args []string) error {
	jsonMode, _ := cmd.Flags().GetBool("json")
	// Trimmed: dial.Sniff trims for its own classification but the raw
	// string would otherwise still reach nipcash.Decode, which rejects a
	// stray leading/trailing space as invalid bech32.
	input, embeddedSecret := nipcash.SplitBearerSliceString(strings.TrimSpace(args[0]))

	switch dial.Sniff(input) {
	case dial.KindCircleHub:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that's a Circle Hub connection — use `cashctl join <connection>` to join it"))
	case dial.KindCashHub:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that's a Cash Hub connection (for minting cash) — cashctl can't mint, this needs the Hub operator's own tooling"))
	case dial.KindNWCURI:
		return output.InvalidInputError(cmd, input, fmt.Errorf("that looks like a wallet connection, not a cash token — use `cashctl connect add <name> <uri>` instead"))
	case dial.KindUnknown:
		return output.InvalidInputError(cmd, input, fmt.Errorf("doesn't look like a valid cash token"))
	}

	tok, err := nipcash.Decode(input)
	if err != nil {
		return output.InvalidInputError(cmd, input, err)
	}

	// Local-only guess, used below just to decide whether to bail at Step
	// 0 and what to print pre-network. tok.IdentityRequired is a
	// best-effort hint that can go stale (NIP-CASH §Redemption Metadata)
	// — an embedded secret is checked too since it's unambiguous either
	// way. The save further down never trusts this guess: it uses
	// result.IsBearer, CheckClaim's own live answer.
	isBearer := (tok.IdentityRequired != nil && !*tok.IdentityRequired) || embeddedSecret != ""

	// Step 0: a bearer token with no embedded secret carries nothing this
	// command can act on — degrade to exactly decode --check's own
	// contract (read, optionally cross-check live, never save, never
	// propose securing) rather than erroring. This is an expected
	// outcome, not a usage mistake.
	if isBearer && embeddedSecret == "" {
		printCashBill(jsonMode, tok, isBearer)
		if shouldRunCheck(cmd, jsonMode, false, "Verify online?") {
			checkResult, checkErr := checkClaimOnce(cmd, input, tok)
			if !jsonMode {
				if checkErr == nil {
					fmt.Printf("check: matches (%s)\n", output.FormatAmount(int64(checkResult.AmountMillis)))
				} else {
					fmt.Printf("check: %s\n", checkErr)
				}
			}
		}
		if jsonMode {
			output.PrintJSON(map[string]any{"received": false, "reason": "no_embedded_secret"})
			return nil
		}
		fmt.Println()
		fmt.Println(`No spending secret — ask for the full "token#secret" string, or run "cashctl decode".`)
		return nil
	}

	l, err := ledger.Load()
	if err != nil {
		return output.RuntimeError(cmd, err)
	}
	if _, ok := l.FindByToken(input); ok {
		return output.ConflictError(cmd, input, ledger.ErrAlreadyHeld)
	}

	printCashBill(jsonMode, tok, isBearer)

	result, err := checkClaimWithCashHub(cmd, input, tok)
	if err != nil {
		return err
	}

	entry := ledger.Entry{
		Token:        input,
		WalletPubkey: tok.WalletPubkey,
		Secret:       tok.Secret,
		BearerSecret: embeddedSecret,
		RelayURLs:    tok.RelayURLs,
		// result.IsBearer, not tok.IdentityRequired or the guess above:
		// this drives every later operation on the entry, so it should
		// carry CheckClaim's live answer, not a pre-network guess.
		IdentityRequired: ptrTo(!result.IsBearer),
		AmountMillis:     &result.AmountMillis,
		Verified:         true,
		MinterPubkey:     result.MinterPubkey,
	}

	added, err := l.Add(entry)
	if err != nil {
		if errors.Is(err, ledger.ErrAlreadyHeld) {
			return output.ConflictError(cmd, input, err)
		}
		return output.RuntimeError(cmd, err)
	}
	l.AppendHistory("receive", fmt.Sprintf("received %s", output.FormatAmount(int64(result.AmountMillis))))

	if err := l.Save(); err != nil {
		// A concurrent `cashctl receive` of this same token: both
		// processes' Add checks ran against their own pre-Save in-memory
		// snapshot and saw nothing, so only the DB's own token-uniqueness
		// constraint catches the loser here, as ledger.ErrAlreadyHeld
		// (see Save's own doc comment on this path) — same classification
		// and message as the ordinary already-held case above, not a raw
		// SQL error.
		if errors.Is(err, ledger.ErrAlreadyHeld) {
			return output.ConflictError(cmd, input, err)
		}
		return output.RuntimeError(cmd, err)
	}

	if !jsonMode {
		fmt.Printf("Received %s.\n", output.FormatAmount(int64(result.AmountMillis)))
	}

	// result.IsBearer, not the pre-check guess above.
	protected, finalEntry := protectBearerReceipt(cmd, l, added, result.IsBearer)

	if jsonMode {
		// JSON key stays "secured" — an intentional, unchanged part of the
		// --json contract (only the human-mode wording/identifiers changed).
		output.PrintJSON(map[string]any{"entry": finalEntry, "secured": protected.json()})
	}
	return nil
}

// printCashBill prints a token's details before receive commits to
// anything — the human gets to see what they're about to accept, in the
// same moment cashctl is about to go check it's real. Never prints Secret
// or the bearer secret: this is display, not a way to extract a working
// credential (see decode.go's own "pairing secret is never included"
// rule).
func printCashBill(jsonMode bool, tok nipcash.Token, isBearer bool) {
	output.Linef(jsonMode, "Cash bill:")
	output.Linef(jsonMode, "  wallet_pubkey: %s", tok.WalletPubkey)
	if len(tok.RelayURLs) > 0 {
		output.Linef(jsonMode, "  relays: %s", joinStrings(tok.RelayURLs))
	}
	switch {
	case isBearer:
		output.Linef(jsonMode, "  identity: bearer-mode")
	case tok.IdentityRequired != nil:
		output.Linef(jsonMode, "  identity: requires proof")
	default:
		output.Linef(jsonMode, "  identity: unspecified")
	}
	minterLine, amountLine := formatMinterStatus(tok)
	output.Linef(jsonMode, "  %s", minterLine)
	if amountLine != "" {
		output.Linef(jsonMode, "  %s", amountLine)
	}
}

// minterPubkeyFromToken resolves the Entry.MinterPubkey to persist at
// receive time: nil for a token with no mint-provenance pair, or one whose
// signature doesn't verify — only a token VerifyProvenance actually
// confirms gets a trustworthy minter identity recorded (see ledger.Entry's
// own doc comment on why this matters for cash selection).
func minterPubkeyFromToken(tok nipcash.Token) *string {
	if !tok.HasProvenance() {
		return nil
	}
	minter, valid := nipcash.VerifyProvenance(tok)
	if !valid {
		return nil
	}
	return &minter
}

// checkClaimWithCashHub is receive's mandatory "is this bill real" step —
// dials the token's own Cash Hub connection and confirms a matching,
// unclaimed recipient actually exists there via nipcash/client's shared
// CheckClaim, animated with WithSpinner since it's a network round trip.
// Any failure here — the Cash Hub is unreachable, or reachable but has no
// recipient matching this token's identity — refuses the token outright:
// nothing gets saved, ever, on the strength of the token string alone.
// Always passes its own local pubkey (best-effort): CheckClaim tries
// both pubkey and bearer live, so the caller doesn't need to guess.
func checkClaimWithCashHub(cmd *cobra.Command, input string, tok nipcash.Token) (*nipcash.CheckClaimResult, error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	var result *nipcash.CheckClaimResult
	noMatch := false
	err := WithSpinner(jsonMode, "Checking...", func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		client, err := nipcashclient.Connect(ctx, input)
		if err != nil {
			return err
		}
		defer client.Close()

		myPubHex, _ := localPubKeyHex(cmd) // best-effort; empty means no pubkey match attempted
		r, err := client.CheckClaim(ctx, tok, myPubHex)
		if errors.Is(err, nipcash.ErrClaimNotFound) {
			noMatch = true
			return fmt.Errorf("the Cash Hub has no matching recipient for this token — refusing to receive it")
		}
		if err != nil {
			return err
		}
		result = r
		return nil
	})
	if err == nil {
		return result, nil
	}
	if noMatch {
		return nil, output.InvalidInputError(cmd, input, err)
	}
	return nil, output.NetworkError(cmd, err)
}

// checkClaimOnce is Step 0's optional, non-fatal check for a bearer token
// with no embedded secret — same underlying CheckClaim call as
// checkClaimWithCashHub, but never classified into a hard CLIError:
// there's nothing to save regardless of what this reports, so the
// caller just prints whatever comes back (a match, a miss, or a dial
// failure) and moves on.
func checkClaimOnce(cmd *cobra.Command, input string, tok nipcash.Token) (*nipcash.CheckClaimResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, input)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.CheckClaim(ctx, tok, nipcash.NoLocalIdentity)
}
