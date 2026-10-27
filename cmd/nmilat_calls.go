package cmd

import (
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
)

// billJournal is the write-ahead journal and device id every bill-creating call
// (cash_transfer, cash_consolidate, mint_cash) is placed with. nmilat refuses
// such a call without a journal (ErrNoJournal) before anything is sent, so until
// the ledger's journal exists (pending_calls, schema v2) these calls are refused
// with "not placed — your money has not moved".
func billJournal() (nipcashclient.Journal, string) {
	return nil, ""
}
