// Command decode-npub is a tiny dev-only helper for bin/run.sh's
// prepare_r2_circle_join step: decode an npub1... string to its 32-byte
// hex pubkey, so the harness (on the host, not inside the agent
// container) can authorize the agent's own freshly generated identity
// under an ephemeral circle_hub's allowlist before that round proceeds.
// `decode-npub --new` instead prints a fresh third party's identity as
// {"npub","hex"}, for a round that sends to someone who is not the agent.
// Not part of cashctl itself — run with `go run`, never built/shipped.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/utils"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--new" {
		newIdentity()
		return
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: decode-npub <npub1...> | decode-npub --new")
		os.Exit(2)
	}
	hrp, data, err := bech32.DecodeToBase256(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode npub: %v\n", err)
		os.Exit(1)
	}
	if hrp != "npub" {
		fmt.Fprintf(os.Stderr, "not an npub (hrp=%q)\n", hrp)
		os.Exit(1)
	}
	fmt.Println(hex.EncodeToString(data))
}

func newIdentity() {
	priv := make([]byte, 32)
	if _, err := rand.Read(priv); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pub, err := utils.GetPublicKey(hex.EncodeToString(priv))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	raw, _ := hex.DecodeString(pub)
	bits, err := bech32.ConvertBits(raw, 8, 5, true)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	npub, err := bech32.Encode("npub", bits)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"npub": npub, "hex": pub})
}
