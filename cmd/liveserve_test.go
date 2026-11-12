package cmd

import (
	"context"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
)

// TestLiveServeForXClient serves a cashctl pocket (one 40 000 held bill, key
// testPocketKey) over a real WebSocket on an ephemeral loopback port, writes the
// ws:// URL to $XSERVE_URLFILE, and blocks until $XSERVE_STOPFILE appears (or 30s).
//
// It is the Go half of the cross-client LIVE-SOCKET proof: the cashkit vitest
// test/xclient-live.test.ts dials this exact URL, answers the recipient-only
// NIP-42 AUTH as the owner (same key), fetches the kind-1059 wrap and resolves
// A's bill — all over the wire. The byte-level content interop is already proven
// (xclient_interop_test.go ↔ cashkit xclient-interop.test.ts); this proves the
// TRANSPORT (WS framing + NIP-42 AUTH handshake) between the two implementations.
//
// Env-guarded so it is a no-op under the normal `go test` sweep (it only runs
// when $XSERVE_URLFILE is set, i.e. when the cross-client harness drives it).
func TestLiveServeForXClient(t *testing.T) {
	urlFile := os.Getenv("XSERVE_URLFILE")
	if urlFile == "" {
		t.Skip("XSERVE_URLFILE unset — cross-client live-socket harness not driving this run")
	}
	stopFile := os.Getenv("XSERVE_STOPFILE")

	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()

	const amt = 40_000
	held, w := heldBill(t, amt)
	t.Logf("serving held bill w=%s amount=%d", w, amt)

	p, err := pocket.Open(ctx, t.TempDir(), testPocketKey, "flokicoin")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	// Listen first so the handler's advertised NIP-42 relay URL matches the
	// address the cashkit client dials (the AUTH event's relay tag must match).
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "ws://" + ln.Addr().String()
	handler, err := pocketServeHandler(ctx, p, []ledger.Entry{held}, url)
	if err != nil {
		t.Fatal(err)
	}
	defer handler.VerificationWorker.Stop()
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	if err := os.WriteFile(urlFile, []byte(url), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("serving on %s (url written to %s) — waiting for client", url, urlFile)

	// Block until the client signals done (stop file) or the deadline.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if stopFile != "" {
			if _, err := os.Stat(stopFile); err == nil {
				t.Logf("stop file seen — client done, shutting serve")
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
	t.Logf("deadline reached without stop file — shutting serve")
}
