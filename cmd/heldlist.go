package cmd

import "time"

// formatReceivedDate renders a ledger.Entry's ReceivedAt (RFC3339) as a
// short human date for terminal listings — falls back to the raw string
// on a parse failure rather than erroring, since this is display-only.
func formatReceivedDate(receivedAt string) string {
	t, err := time.Parse(time.RFC3339, receivedAt)
	if err != nil {
		return receivedAt
	}
	return t.Format("Jan 2")
}
