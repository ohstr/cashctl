package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/config"
)

// zapVectors is nmilat's testdata/vectors/zap.json (NIP-CZ), copied to
// testdata/vectors/zap.json.
type zapVectors struct {
	Target nip01.Event `json:"target"`
	Info   []struct {
		Event nip01.Event `json:"event"`
	} `json:"info"`
	Parse []struct {
		Name  string          `json:"name"`
		Event json.RawMessage `json:"event"`
		Rule  int             `json:"rule"`
		Error string          `json:"error"`
	} `json:"parse"`
	Verify []struct {
		Name    string          `json:"name"`
		Event   json.RawMessage `json:"event"`
		Info    bool            `json:"withInfo"`
		Trusted []string        `json:"trustedMinters"`
		Rules   []struct {
			N       int    `json:"n"`
			OK      bool   `json:"ok"`
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		} `json:"rules"`
		Minter    string `json:"minter"`
		Countable bool   `json:"countable"`
		Reason    string `json:"reason"`
	} `json:"verify"`
}

func loadZapVectors(t *testing.T) zapVectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/vectors/zap.json")
	if err != nil {
		t.Fatal(err)
	}
	var v zapVectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Parse) == 0 || len(v.Verify) == 0 || len(v.Info) == 0 {
		t.Fatal("zap.json has no cases")
	}
	return v
}

// decodeZapJSON runs `decode --json` on a Cash Zap with its target event, the
// recipient's 10522 read from info (nil: none).
func decodeZapJSON(t *testing.T, ev json.RawMessage, target nip01.Event, info *nipCZ.CashZapInfo) (map[string]any, error) {
	t.Helper()
	prev := zapInfoFn
	zapInfoFn = func(context.Context, []string, string) (*nipCZ.CashZapInfo, []string) { return info, nil }
	t.Cleanup(func() { zapInfoFn = prev })
	tb, _ := json.Marshal(target)
	tf := filepath.Join(t.TempDir(), "target.json")
	if err := os.WriteFile(tf, tb, 0o600); err != nil {
		t.Fatal(err)
	}
	c := zapCmd(newDecodeCmd(), map[string]string{"target": tf, "relay": "wss://relay.example.com"})
	c.SetArgs([]string{string(ev)})
	var err error
	stdout, _ := captureStreams(t, func() { err = c.Execute() })
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if jerr := json.Unmarshal([]byte(stdout), &doc); jerr != nil {
		t.Fatalf("not JSON: %q", stdout)
	}
	return doc, nil
}

// NIP-CZ rules 1-8 and counting: every verify case of the shared vectors reads
// the same through `cashctl decode`.
func TestDecodeZap_SharedVerifyVectors(t *testing.T) {
	v := loadZapVectors(t)
	info, err := nipCZ.ParseCashZapInfo(&v.Info[0].Event)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range v.Verify {
		t.Run(c.Name, func(t *testing.T) {
			appdir.SetOverride(t.TempDir())
			t.Cleanup(func() { appdir.SetOverride("") })
			for _, m := range c.Trusted {
				if err := config.TrustMinter(config.TrustedMinter{MinterXOnly: m, Chain: "bitcoin"}); err != nil {
					t.Fatal(err)
				}
			}
			var i *nipCZ.CashZapInfo
			if c.Info {
				i = info
			}
			doc, err := decodeZapJSON(t, c.Event, v.Target, i)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if doc["countable"] != c.Countable || doc["minter_pubkey"] != c.Minter {
				t.Errorf("countable %v minter %v; want %v %s", doc["countable"], doc["minter_pubkey"], c.Countable, c.Minter)
			}
			if got, _ := doc["reason"].(string); got != c.Reason {
				t.Errorf("reason %q, want %q", got, c.Reason)
			}
			rows, _ := doc["rules"].([]any)
			if len(rows) != len(c.Rules) {
				t.Fatalf("%d rules, want %d", len(rows), len(c.Rules))
			}
			for k, r := range c.Rules {
				got := rows[k].(map[string]any)
				reason, _ := got["reason"].(string)
				if got["n"] != float64(r.N) || got["ok"] != r.OK || (!r.OK && reason != r.Reason) {
					t.Errorf("rule %d: %v; want ok=%v %q", r.N, got, r.OK, r.Reason)
				}
			}
		})
	}
}

// NIP-CZ's strict tag rules (K-CZ-1..6): each parse case passes or fails at the
// rule the vectors name, and every rule before it passes.
func TestDecodeZap_SharedParseVectors(t *testing.T) {
	v := loadZapVectors(t)
	for _, c := range v.Parse {
		t.Run(c.Name, func(t *testing.T) {
			appdir.SetOverride(t.TempDir())
			t.Cleanup(func() { appdir.SetOverride("") })
			doc, err := decodeZapJSON(t, c.Event, v.Target, nil)
			if err != nil {
				if c.Rule == 0 {
					t.Fatalf("a valid zap was refused: %v", err)
				}
				return // refused before any rule is shown: not a Cash Zap at all
			}
			rows, _ := doc["rules"].([]any)
			for _, row := range rows {
				r := row.(map[string]any)
				n := int(r["n"].(float64))
				reason, _ := r["reason"].(string)
				switch {
				case c.Rule == 0 && n <= 2 && r["ok"] != true:
					t.Errorf("rule %d failed on a valid zap: %s", n, reason)
				case c.Rule > 0 && n < c.Rule && r["ok"] != true:
					t.Errorf("rule %d failed before rule %d: %s", n, c.Rule, reason)
				case c.Rule > 0 && n == c.Rule:
					if r["ok"] != false {
						t.Errorf("rule %d passed, want it to fail (%s)", n, c.Error)
					} else if want := c.Error[strings.LastIndex(c.Error, ": ")+2:]; c.Error != "" && !strings.Contains(reason, want) {
						t.Errorf("rule %d: %q, want %q", n, reason, want)
					}
				}
			}
		})
	}
}
