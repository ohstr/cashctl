package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/spf13/cobra"

	"github.com/ohstr/cashctl/internal/output"
)

func testBech32(t *testing.T, hrp string) string {
	t.Helper()
	data, err := bech32.ConvertBits(make([]byte, 40), 8, 5, true)
	if err != nil {
		t.Fatal(err)
	}
	s, err := bech32.Encode(hrp, data)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// decode and receive name a pasted Lightning invoice, with its amount, and say
// what to do with it, instead of failing on its bytes as a cash token.
func TestDecodeAndReceive_NameALightningInvoice(t *testing.T) {
	invoice := testBech32(t, "lnfc600n")
	for name, cmd := range map[string]*cobra.Command{"decode": newDecodeCmd(), "receive": newCashReceiveCmd()} {
		cmd.SetArgs([]string{invoice})
		cmd.SetOut(io.Discard)
		var err error
		_, _ = captureStreams(t, func() { err = cmd.Execute() })
		if ce := output.AsCLIError(err); ce.Code != output.CodeInvalidInput {
			t.Errorf("%s: code = %q, want invalid_input: %v", name, ce.Code, err)
		}
		if err == nil || !strings.Contains(err.Error(), "Lightning invoice for") || !strings.Contains(err.Error(), "cashctl pay") {
			t.Errorf("%s: %v does not name the invoice and its amount", name, err)
		}
	}
}

func TestLightningMessage_LNURL(t *testing.T) {
	if got := lightningMessage(testBech32(t, "lnurl")); !strings.Contains(got, "LNURL") {
		t.Errorf("lightningMessage(lnurl) = %q, want it named as an LNURL", got)
	}
}
