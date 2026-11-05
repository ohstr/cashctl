package cmd

import (
	"reflect"
	"testing"

	"github.com/ohstr/nmilat/nipCZ"

	"github.com/ohstr/cashctl/internal/config"
)

// Minter x-only keys (64 lowercase hex); the builder emits them as "02"+xonly.
const (
	xA = "79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"
	xB = "c6047f9441ed7d6d3045406e95c07cd85c778e4b8cef3ca7abac09b95c709ee5"
	xC = "f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"
)

func m(x, chain string) config.TrustedMinter {
	return config.TrustedMinter{MinterXOnly: x, Chain: chain}
}

// The builder is pure: correct kind/content, chain tags = distinct minter
// chains (plus --chain), each mint = "02"+xonly with its chain, relay tags
// present, and a deterministic, sorted tag order regardless of input order.
func TestBuildCashZapInfo(t *testing.T) {
	cases := []struct {
		name    string
		minters []config.TrustedMinter
		chains  []string
		relays  []string
		want    [][]string
	}{
		{
			name: "empty",
			want: [][]string{},
		},
		{
			name:    "chains mints and relays, sorted",
			minters: []config.TrustedMinter{m(xB, "bitcoin"), m(xA, "bitcoin"), m(xC, "flokicoin")},
			relays:  []string{"wss://b.example", "wss://a.example"},
			want: [][]string{
				{"chain", "bitcoin"},
				{"chain", "flokicoin"},
				{"mint", "02" + xA, "bitcoin"},
				{"mint", "02" + xB, "bitcoin"},
				{"mint", "02" + xC, "flokicoin"},
				{"relay", "wss://a.example"},
				{"relay", "wss://b.example"},
			},
		},
		{
			name:    "extra chain with no minter is included",
			minters: []config.TrustedMinter{m(xA, "bitcoin")},
			chains:  []string{"flokicoin"},
			want: [][]string{
				{"chain", "bitcoin"},
				{"chain", "flokicoin"},
				{"mint", "02" + xA, "bitcoin"},
			},
		},
		{
			name:    "duplicate minter and relay are deduped",
			minters: []config.TrustedMinter{m(xA, "bitcoin"), m(xA, "bitcoin")},
			relays:  []string{"wss://a.example", "wss://a.example"},
			want: [][]string{
				{"chain", "bitcoin"},
				{"mint", "02" + xA, "bitcoin"},
				{"relay", "wss://a.example"},
			},
		},
		{
			name:    "a chain with no trusted minter is not auto-included",
			minters: []config.TrustedMinter{m(xA, "bitcoin")},
			want: [][]string{
				{"chain", "bitcoin"},
				{"mint", "02" + xA, "bitcoin"},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := buildCashZapInfo(tc.minters, tc.chains, tc.relays)
			if ev.Kind != nipCZ.KindCashZapInfo {
				t.Errorf("kind %d, want %d", ev.Kind, nipCZ.KindCashZapInfo)
			}
			if ev.Content != "" {
				t.Errorf("content %q, want empty", ev.Content)
			}
			if !reflect.DeepEqual(ev.Tags, tc.want) {
				t.Errorf("tags\n got %v\nwant %v", ev.Tags, tc.want)
			}
		})
	}
}

// The same inputs in a different order give the identical event.
func TestBuildCashZapInfo_Deterministic(t *testing.T) {
	a := buildCashZapInfo([]config.TrustedMinter{m(xA, "bitcoin"), m(xC, "flokicoin")}, []string{"bitcoin"}, []string{"wss://a.example", "wss://b.example"})
	b := buildCashZapInfo([]config.TrustedMinter{m(xC, "flokicoin"), m(xA, "bitcoin")}, []string{"flokicoin"}, []string{"wss://b.example", "wss://a.example"})
	if !reflect.DeepEqual(a.Tags, b.Tags) {
		t.Errorf("not deterministic:\n%v\n%v", a.Tags, b.Tags)
	}
}
