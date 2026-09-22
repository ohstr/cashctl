package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// entryRecoveryString is the exact string a human needs to reach e again —
// the bare token for a pubkey/connection-key entry (the local identity
// already controls it), or the combined <token>#<secret> gift for a
// cash-mode one (nothing else grants access to it at all). Used only when a
// Hub-side mutation succeeded but persisting its result locally then
// failed — the money is real and reachable, but about to become
// undiscoverable the instant this process exits unless it's printed now.
func entryRecoveryString(e *ledger.Entry) string {
	if e == nil || e.Token == "" {
		return ""
	}
	if e.CashSecret != "" {
		return e.Token + "#" + e.CashSecret
	}
	return e.Token
}

// entryRecoveryHint wraps entryRecoveryString as a ready-to-print sentence
// fragment for reportUnsavedResult's recoveryHint — "" (nothing to add)
// when e or its token is unavailable.
func entryRecoveryHint(e *ledger.Entry) string {
	s := entryRecoveryString(e)
	if s == "" {
		return ""
	}
	return "The resulting token — save this now, it will not be shown again: " + s
}

// reportUnsavedResult builds the error for the specific, worse-than-usual
// failure where a Hub-side mutation is CONFIRMED to have happened —
// verb names it ("Consolidate", "Redeem", ...) — but the follow-up
// ledger.Save() that was supposed to record it then failed. Unlike an
// ordinary decline, this is never safe to let read as a generic failure:
// the money already moved, the local wallet just doesn't know it yet, and
// --yes/--json automation must not treat retrying (or a prior "success")
// as sensible next steps. recoveryHint is whatever the caller can offer to
// make the mismatch findable again — entryRecoveryString's result, a
// payment preimage, or "" if genuinely nothing more specific exists beyond
// pointing at wallet show / the Hub's own records.
func reportUnsavedResult(cmd *cobra.Command, saveErr error, verb, recoveryHint string) error {
	msg := fmt.Sprintf("%s succeeded on the Hub, but saving that locally failed (%v) — your wallet's local record does not match reality.", verb, saveErr)
	if recoveryHint != "" {
		msg += " " + recoveryHint
	} else {
		msg += " Check `cashctl wallet show`/`cashctl cash list-recipients` and try again before assuming anything failed."
	}
	return output.RuntimeError(cmd, fmt.Errorf("%s", msg))
}
