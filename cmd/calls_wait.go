package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ohstr/nmilat/nipcash"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// A call answered with a code other than BAD_REQUEST, or not answered at all,
// stays pending and is looked up after its deadline (plan §10.3 Errors).
// cashctl waits for that in the same invocation, up to deadline + 1 and never
// more than maxPlacementWait, unless --no-wait: a lookup answered done is
// applied exactly as the reply would have been, and one answered unknown after
// a code closes the call not applied with that code's own exit (§10.4).
// Anything else leaves the call open for `cash settle` (exit 8).

// maxPlacementWait bounds the in-invocation wait.
const maxPlacementWait = 62 * time.Second

// placementWaitEnabled is false in unit tests, which opt in one at a time.
var placementWaitEnabled = true

// placementSleep waits d. Swapped in tests.
var placementSleep = time.Sleep

// awaitPlacement waits for c's deadline and looks it up once. raw is the
// result of a call looked up done; closedErr the error of one closed not
// applied. Both empty: the call stays open.
func awaitPlacement(_ context.Context, cmd *cobra.Command, l *ledger.Ledger, c *ledger.Call) (raw json.RawMessage, closedErr error) {
	if !placementWaitEnabled || c == nil || c.State != ledger.CallLookup {
		return nil, nil
	}
	if noWait, _ := cmd.Flags().GetBool("no-wait"); noWait {
		return nil, nil
	}
	wait := time.Until(time.Unix(c.Deadline+1, 0))
	if wait > maxPlacementWait {
		return nil, nil
	}
	jsonMode, _ := cmd.Flags().GetBool("json")
	if wait > 0 {
		_ = WithSpinner(jsonMode, fmt.Sprintf("No clear answer; asking the Hub at call %s's deadline (%ds)...", c.CallID, int(wait.Seconds())+1), func() error {
			placementSleep(wait)
			return nil
		})
	}
	lctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := lookupCallFn(lctx, cmd, l, c)
	if err != nil {
		return nil, nil
	}
	switch out.Status {
	case nipcash.LookupDone:
		return out.Result, nil
	case nipcash.LookupUnknown:
		if c.FirstError == "" {
			// Never seen: `cash settle` places it again under its derived id.
			return nil, nil
		}
		code := c.FirstError
		state := markRefusedNotFound(l, c, code)
		closeCall(l, c, ledger.CallNotApplied, code)
		_ = l.Save()
		if state != "" {
			return nil, goneError(cmd, c.SourceEntryIDs[0], state)
		}
		return nil, output.NWCErrorForCashToken(cmd, &relayclient.WalletError{Method: c.Method, Code: code,
			Message: "the Hub refused it (" + code + ") and never applied it; nothing moved"})
	}
	return nil, nil
}
