package cmd

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nipcash"
)

func TestCashNoteRekeyedAway(t *testing.T) {
	mine, other := strings.Repeat("a", 64), strings.Repeat("b", 64)
	commit := func(s string) string {
		c, err := nipcash.CashCommitment(s)
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	row := func(typ, value string) *nipcash.CashStatusResult {
		return &nipcash.CashStatusResult{Recipients: []nipcash.RecipientStatus{{IdentityType: typ, IdentityValue: value}}}
	}
	for _, c := range []struct {
		name   string
		r      *nipcash.CashStatusResult
		secret string
		want   bool
	}{
		{"still ours", row("cash", commit(mine)), mine, false},
		{"re-keyed by someone else", row("cash", commit(other)), mine, true},
		{"no cash secret in use", row("cash", commit(other)), "", false},
		{"a pubkey row", row("pubkey", strings.Repeat("c", 64)), mine, false},
		{"a tombstone", &nipcash.CashStatusResult{Tombstone: &nipcash.Tombstone{}}, mine, false},
		{"no answer", nil, mine, false},
	} {
		if got := cashNoteRekeyedAway(c.r, c.secret); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
