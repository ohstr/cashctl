package pocket

// Cross-client interop proof (fork, not committed): cashctl (via the same
// nipCP.Seal its pocket projection uses) emits a kind-1059 gift-wrapped held-bill
// 9525 for a known key; the companion cashkit test (test/xclient-interop.test.ts)
// unwraps + parses + resolves THESE EXACT bytes with the same key. Run on demand:
//   XCLIENT_OUT=/tmp/ccpk-xclient GOWORK=off go test ./internal/pocket -run TestXClientEmitWrap -v
import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/utils"
)

func TestXClientEmitWrap(t *testing.T) {
	out := os.Getenv("XCLIENT_OUT")
	if out == "" {
		t.Skip("set XCLIENT_OUT to emit the cross-client wrap")
	}
	const priv = "1111111111111111111111111111111111111111111111111111111111111111" // == cashkit seed 0x11
	pub, err := utils.GetPublicKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	e := nipCP.Entry{
		W:         pub, // valid 64-hex wallet pubkey (a bill addressed to the owner)
		Rev:       1,
		Cash:      "satscash1xclientinteropheldbillproof",
		Amount:    50000,
		Chain:     "bitcoin",
		Identity:  nipCP.EntryIdentity{Type: nipCP.IdentityPubkey, Value: pub},
		State:     nipCP.StateLive,
		CheckedAt: 1760000000,
	}
	wrap, _, err := nipCP.Seal(e, priv)
	if err != nil {
		t.Fatalf("nipCP.Seal: %v", err)
	}
	if wrap.Kind != 1059 {
		t.Fatalf("want a 1059 gift-wrap, got kind %d", wrap.Kind)
	}
	if err := os.MkdirAll(out, 0o700); err != nil {
		t.Fatal(err)
	}
	writeJSON(t, filepath.Join(out, "wrap.json"), wrap)
	writeJSON(t, filepath.Join(out, "expected.json"), map[string]any{
		"w": e.W, "amount": e.Amount, "state": "live", "chain": e.Chain, "ownerPub": pub,
	})
	t.Logf("emitted 1059 wrap id=%s (w=%s amount=%d) to %s", wrap.ID, e.W, e.Amount, out)
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
