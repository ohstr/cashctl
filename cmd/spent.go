package cmd

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/output"
)

// errBillSpent is what every path says once a bill's Hub has answered with a
// tombstone.
var errBillSpent = errors.New("the Hub says this bill is spent: its value has already moved and the bill is gone")

// recordSpent marks a held entry spent after its Hub answered with a tombstone
// (NIP-CASH §Answering About a Destroyed Bill), so `wallet balance` and
// `redeem --all` stop offering it, and returns the spent error. Only ever
// called on a tombstone: silence proves nothing (NIP-CASH: a client MUST NOT
// report a bill spent on a timeout alone).
func recordSpent(cmd *cobra.Command, l *ledger.Ledger, entry *ledger.Entry) error {
	markSpent(l, entry)
	return output.SpentError(cmd, entry.ID, errBillSpent)
}

// markSpent is recordSpent without the error, for a read that succeeded.
func markSpent(l *ledger.Ledger, entry *ledger.Entry) {
	if l == nil || entry == nil || entry.Status != ledger.StatusHeld {
		return
	}
	_ = l.SetStatus(entry.ID, ledger.StatusSpent)
	l.AppendHistory("spent", fmt.Sprintf("the Hub reported token %s spent", entry.ID))
	_ = l.Save() // best-effort: the next run asks the Hub again and gets the same tombstone
}

// alreadySpentError refuses an explicitly named entry that is no longer held.
//
// Spent, the same answer the Hub's tombstone gives, for a bill reported spent
// and for one this wallet redeemed or merged away: those statuses are written
// only on the Hub's own confirmation, after which the Hub deletes the bill and
// answers a tombstone for its retention window (NIP-CASH §Answering About a
// Destroyed Bill). Asking it again would only add a network call, and past the
// window its silence. not_found for a bill transferred away: it still exists on
// the Hub, under its new holder, and is simply no longer yours.
func alreadySpentError(cmd *cobra.Command, id, status string) error {
	switch status {
	case ledger.StatusSpent, ledger.StatusRedeemed, ledger.StatusConsolidated:
		return output.SpentError(cmd, id, fmt.Errorf("token %q was already %s, so it is spent: its value has already moved", id, spentStatusDescription(status)))
	}
	return output.NotFoundError(cmd, id, fmt.Errorf("token %q was already %s, so it is no longer yours", id, spentStatusDescription(status)))
}
