package cmd

import (
	"errors"
	"fmt"
	"strings"

	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

// classifyNWCErr turns a wallet-call error into cashctl's own classified
// *CLIError: a *relayclient.WalletError becomes output.NWCError (plain-
// language translation, correct exit code); anything else (a dial/network
// failure) becomes output.NetworkError.
func classifyNWCErr(cmd *cobra.Command, err error) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCError(cmd, walletErr)
	}
	return output.NetworkError(cmd, err)
}

// classifyCashTokenNWCErr is classifyNWCErr's counterpart for a call that
// checks or spends a cash token's own claim (CheckClaim, CashRedeem,
// CashTransfer, ListRecipients — anything dialing entry.Token itself)
// rather than a registered wallet connection — see
// output.NWCErrorForCashToken's own doc comment for why EXPIRED needs
// different text there.
func classifyCashTokenNWCErr(cmd *cobra.Command, err error) error {
	var walletErr *relayclient.WalletError
	if errors.As(err, &walletErr) {
		return output.NWCErrorForCashToken(cmd, walletErr)
	}
	return output.NetworkError(cmd, err)
}

// isAmbiguousDeliveryErr reports whether err is the shape of a specific,
// known Hub/SDK interop failure (see docs/private's own audit of this):
// the Hub answers a cash_consolidate/mint_cash call for real, but this
// client then fails to decrypt/parse the delivery it sent back —
// "decrypt delivery" is nipcash's own error prefix for exactly that
// decode step (nipcash.CashConsolidateParams.ParseResult and its
// siblings), which only ever runs on a raw response the Hub actually
// returned. A plain decline (bad request, restricted, an ordinary
// network failure) never reaches that far, so matching on it isn't a
// guess about WHETHER the Hub executed the request — only about whether
// a caller holding sources it's about to give up on should be warned
// that it might have.
//
// There is deliberately no attempt here to go further and DECIDE that
// the sources were consumed (e.g. marking them accordingly): the SDK
// exposes no signal more specific than this message for cash_consolidate
// (unlike RekeyBearerSlice/TransferFromSources' own typed
// PartialProgressError for their composite calls), so guessing either
// way is worse than saying plainly that it's unknown — guessing
// "consumed" when it wasn't would lock a caller out of genuinely
// still-spendable funds, exactly the class of bug this whole check
// exists to avoid on the other side.
func isAmbiguousDeliveryErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "decrypt delivery")
}

// warnAmbiguousDelivery wraps err with the honest, actionable uncertainty
// isAmbiguousDeliveryErr's own doc comment describes, when it applies —
// unchanged otherwise. Wraps err itself (not classifyNWCErr's output), so
// the added text survives into CLIError.Err.Error(), what EmitError
// actually prints in both modes — a naming distinct from "warn" that
// doesn't classify anything: exit code / retryable stay exactly what err
// already implied.
func warnAmbiguousDelivery(err error, sourceIDs []string) error {
	if !isAmbiguousDeliveryErr(err) {
		return err
	}
	which := "these sources"
	if len(sourceIDs) > 0 {
		which = strings.Join(sourceIDs, ", ")
	}
	return fmt.Errorf("%w — the Hub may have already merged %s even though this reply couldn't be read; check each with `cashctl cash list-recipients --token <id>` or `cashctl decode --check <token>` before assuming it's still spendable, and don't just retry", err, which)
}
