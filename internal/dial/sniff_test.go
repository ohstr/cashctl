package dial

import (
	"testing"

	"github.com/flokiorg/go-flokicoin/chainutil/bech32"
	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nipIC"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcw"
)

func TestSniff_NWCURI(t *testing.T) {
	if got := Sniff("nostr+walletconnect://abc?relay=wss://relay.example&secret=xyz"); got != KindNWCURI {
		t.Errorf("Sniff() = %v, want KindNWCURI", got)
	}
}

func TestSniff_CashToken(t *testing.T) {
	token, err := nipcash.Encode(nipcash.Token{
		HRP:          "lokicash",
		WalletPubkey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Secret:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("test setup: Encode() error = %v", err)
	}
	if got := Sniff(token); got != KindCashToken {
		t.Errorf("Sniff(%q) = %v, want KindCashToken", token, got)
	}
}

func TestSniff_CircleHub(t *testing.T) {
	s, err := nipcw.EncodeCircleHubConnection(nipcw.CircleHubConnection{
		WalletPubkey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Secret:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("test setup: EncodeCircleHubConnection() error = %v", err)
	}
	if got := Sniff(s); got != KindCircleHub {
		t.Errorf("Sniff(%q) = %v, want KindCircleHub", s, got)
	}
}

func TestSniff_CashHub(t *testing.T) {
	s, err := nipcash.EncodeCashHubConnection(nipcash.CashHubConnection{
		WalletPubkey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Secret:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("test setup: EncodeCashHubConnection() error = %v", err)
	}
	if got := Sniff(s); got != KindCashHub {
		t.Errorf("Sniff(%q) = %v, want KindCashHub", s, got)
	}
}

// TestSniff_NostrEntity_AllRecognizedHRPs guards against the bug found
// auditing `decode`/`receive`: an npub/nsec/note/nprofile/nevent/naddr/
// nconnection paste used to fall through to nipcash.Decode (which accepts
// any HRP), failing with a cryptic "nipcash: truncated TLV entry at
// offset N" instead of a specific "that's a Nostr X, not a cash token"
// error — worst on nsec (a private key). Every recognized HRP is checked
// generically via a synthetic bech32 string (Sniff/NostrEntityHRP only
// ever look at the HRP itself, never the TLV payload), plus two real
// encoders (nip19, nipIC) to prove this isn't just a synthetic-string
// artifact.
func TestSniff_NostrEntity_AllRecognizedHRPs(t *testing.T) {
	for hrp := range nostrEntityHRPs {
		s, err := bech32.Encode(hrp, []byte{1, 2, 3, 4, 5, 6, 7, 8})
		if err != nil {
			t.Fatalf("bech32.Encode(%q): %v", hrp, err)
		}
		if got := Sniff(s); got != KindNostrEntity {
			t.Errorf("Sniff(%s...) = %v, want KindNostrEntity", hrp, got)
		}
		if got := NostrEntityHRP(s); got != hrp {
			t.Errorf("NostrEntityHRP(%s...) = %q, want %q", hrp, got, hrp)
		}
	}
}

func TestSniff_NostrEntity_RealEncoders(t *testing.T) {
	npub, err := nip19.EncodePublicKey("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if err != nil {
		t.Fatalf("nip19.EncodePublicKey: %v", err)
	}
	if got := Sniff(npub); got != KindNostrEntity {
		t.Errorf("Sniff(npub) = %v, want KindNostrEntity", got)
	}
	if got := NostrEntityHRP(npub); got != "npub" {
		t.Errorf("NostrEntityHRP(npub) = %q, want \"npub\"", got)
	}

	nsec, err := nip19.EncodePrivateKey("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	if err != nil {
		t.Fatalf("nip19.EncodePrivateKey: %v", err)
	}
	if got := Sniff(nsec); got != KindNostrEntity {
		t.Errorf("Sniff(nsec) = %v, want KindNostrEntity", got)
	}
	if got := NostrEntityHRP(nsec); got != "nsec" {
		t.Errorf("NostrEntityHRP(nsec) = %q, want \"nsec\"", got)
	}

	nconn, err := nipIC.EncodeNConnection(
		nipIC.ConnectionKey("a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2"),
		[]string{"wss://relay.example"}, "discord")
	if err != nil {
		t.Fatalf("nipIC.EncodeNConnection: %v", err)
	}
	if got := Sniff(nconn); got != KindNostrEntity {
		t.Errorf("Sniff(nconnection) = %v, want KindNostrEntity", got)
	}
	if got := NostrEntityHRP(nconn); got != "nconnection" {
		t.Errorf("NostrEntityHRP(nconnection) = %q, want \"nconnection\"", got)
	}
}

// TestNostrEntityHRP_EmptyForNonNostrEntity guards against a false
// positive: a cash token, Circle Hub, Cash Hub, or plain unknown string
// must never be misreported as some Nostr entity's HRP.
func TestNostrEntityHRP_EmptyForNonNostrEntity(t *testing.T) {
	token, err := nipcash.Encode(nipcash.Token{
		HRP:          "lokicash",
		WalletPubkey: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Secret:       "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
	})
	if err != nil {
		t.Fatalf("test setup: Encode() error = %v", err)
	}
	for _, s := range []string{token, "not bech32 at all", ""} {
		if got := NostrEntityHRP(s); got != "" {
			t.Errorf("NostrEntityHRP(%.20q) = %q, want empty", s, got)
		}
	}
}

func TestSniff_Unknown(t *testing.T) {
	tests := []string{
		"not a connection string at all",
		"",
		"http://example.com",
		"1234567890",
	}
	for _, in := range tests {
		if got := Sniff(in); got != KindUnknown {
			t.Errorf("Sniff(%q) = %v, want KindUnknown", in, got)
		}
	}
}
