package cmd

import (
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip47"
)

// decode --check reads private join from get_info's circle_wallet.identity and
// names the circle policy in words.
func TestCircleCheck_ReportsPrivateJoinAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name        string
		cw          *nip47.CircleWalletInfo
		wantOffered bool
		wantLines   []string
	}{
		{"offered", &nip47.CircleWalletInfo{Identity: strings.Repeat("ab", 32), CirclePolicy: nip47.CirclePolicyFollowSet},
			true, []string{"private join: offered", "circle policy: follow set"}},
		{"not offered", &nip47.CircleWalletInfo{CirclePolicy: nip47.CirclePolicyAllowlist},
			false, []string{"private join: not offered", "circle policy: allowlist"}},
		{"no circle_wallet", nil, false, []string{"private join: not offered"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := circleJoinableResult(tc.cw)
			if r.PrivateJoin == nil || *r.PrivateJoin != tc.wantOffered {
				t.Errorf("private_join = %v, want %v", r.PrivateJoin, tc.wantOffered)
			}
			printed := withCapturedOutput(func() { printCircleCheck(r) })
			for _, want := range tc.wantLines {
				if !strings.Contains(printed, want+"\n") {
					t.Errorf("printed %q, want a %q line", printed, want)
				}
			}
		})
	}
}

func TestCirclePolicyPhrase_ShowsAnUnknownValueAsSent(t *testing.T) {
	if got := circlePolicyPhrase("following"); got != "following" {
		t.Errorf("circlePolicyPhrase(following) = %q", got)
	}
}
