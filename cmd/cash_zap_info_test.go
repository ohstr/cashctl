package cmd

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/config"
	"github.com/ohstr/cashctl/internal/output"
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

// Publishing is gated: under --json (non-interactive) it refuses unless --yes
// is passed, and with --yes it proceeds.
func TestCashZapInfo_ConsentRefusesJSONWithoutYes(t *testing.T) {
	c := discoverCmd(&cobra.Command{Use: "publish"}, nil) // --json defaults true here
	if ok, err := confirmPublic(c, "publish?"); ok || output.ExitCode(err) != 2 {
		t.Fatalf("--json without --yes must refuse: ok=%v err=%v", ok, err)
	}
	c = discoverCmd(&cobra.Command{Use: "publish"}, map[string]string{"yes": "true"})
	if ok, err := confirmPublic(c, "publish?"); !ok || err != nil {
		t.Fatalf("--json with --yes must proceed: ok=%v err=%v", ok, err)
	}
}

// seedNIP65 caches a kind-10002 for self with read, write and bare relays.
func seedNIP65(t *testing.T, self string) {
	t.Helper()
	list := nip01.NewEvent(10002, "",
		[]string{"r", "wss://read.example", "read"},
		[]string{"r", "wss://write.example", "write"},
		[]string{"r", "wss://both.example"})
	list.PubKey = self
	list.CreatedAt = uint64(time.Now().Unix()) //nolint:gosec // unix time
	raw, _ := json.Marshal(list)
	if err := config.PutDiscoverEvent("nip65-zapinfo", 10002, self, int64(list.CreatedAt), string(raw), config.OriginTrusted, ""); err != nil {
		t.Fatal(err)
	}
}

// show previews the event (no network, no consent); publish signs and sends it
// to the user's write relays, with its tags drawn from the trust store and the
// user's NIP-65 read relays.
func TestCashZapInfo_ShowAndPublish(t *testing.T) {
	f := newSignalFixture(t, true)
	self := localIdentityHex()
	seedNIP65(t, self)
	if err := config.TrustMinter(config.TrustedMinter{MinterXOnly: xA, Chain: "bitcoin"}); err != nil {
		t.Fatal(err)
	}

	// show: a dry run that lists the chains, mints and read relays.
	doc, err := runSignal(t, newCashZapInfoShowCmd(), nil, nil)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if got := jsonStrings(doc["chains"]); !reflect.DeepEqual(got, []string{"bitcoin"}) {
		t.Errorf("chains %v", got)
	}
	if got := jsonStrings(doc["mints"]); !reflect.DeepEqual(got, []string{"02" + xA}) {
		t.Errorf("mints %v", got)
	}
	if got := jsonStrings(doc["relays"]); !reflect.DeepEqual(got, []string{"wss://both.example", "wss://read.example"}) {
		t.Errorf("relays %v", got)
	}
	if len(f.publishedOf(nipCZ.KindCashZapInfo)) != 0 {
		t.Fatal("show must publish nothing")
	}

	// publish: signs and sends, and the result parses back as a valid 10522.
	if _, err := runSignal(t, newCashZapInfoPublishCmd(), nil, map[string]string{"yes": "true"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	sent := f.publishedOf(nipCZ.KindCashZapInfo)
	if len(sent) != 1 {
		t.Fatalf("published %d Cash Zap Infos, want 1", len(sent))
	}
	if sent[0].PubKey != self || sent[0].Content != "" {
		t.Errorf("published by %s content %q", sent[0].PubKey, sent[0].Content)
	}
	info, err := nipCZ.ParseCashZapInfo(sent[0])
	if err != nil {
		t.Fatalf("parse back: %v", err)
	}
	if !slices.Contains(info.Chains, "bitcoin") || !info.ListsMinter("02"+xA, "bitcoin") {
		t.Errorf("info %+v", info)
	}
	if !slices.Contains(info.Relays, "wss://read.example") || !slices.Contains(info.Relays, "wss://both.example") {
		t.Errorf("relays %v", info.Relays)
	}
}

func jsonStrings(v any) []string {
	xs, _ := v.([]any)
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
