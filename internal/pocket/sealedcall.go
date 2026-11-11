package pocket

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"fmt"

	"github.com/ohstr/nmilat/nip44"

	"github.com/ohstr/cashctl/internal/ledger"
)

// A cashctl ledger.Call carries secrets (AuthorizerSecret, Secrets-by-role) and
// its request id in json:"-" fields, so it cannot be JSON-marshalled whole.
// These helpers serialize the FULL value with gob (every exported field) and
// NIP-44 encrypt-to-self, so the in-flight call is stored encrypted at rest in
// pocket-cache.db (the plan's risk 17) and loaded back intact. The sealed string
// is base64(gob) encrypted to the wallet's own key.

// sealCall serializes and encrypts-to-self one ledger.Call.
func sealCall(c ledger.Call, priv, pub string) (string, error) {
	blob, err := gobEncode(c)
	if err != nil {
		return "", fmt.Errorf("pocket: seal call: %w", err)
	}
	sealed, err := nip44.EncryptHex(blob, priv, pub)
	if err != nil {
		return "", fmt.Errorf("pocket: seal call: %w", err)
	}
	return sealed, nil
}

// openCall decrypts and deserializes one ledger.Call.
func openCall(sealed, priv, pub string) (ledger.Call, error) {
	blob, err := nip44.DecryptHex(sealed, priv, pub)
	if err != nil {
		return ledger.Call{}, fmt.Errorf("pocket: open call: %w", err)
	}
	var c ledger.Call
	if err := gobDecode(blob, &c); err != nil {
		return ledger.Call{}, fmt.Errorf("pocket: open call: %w", err)
	}
	return c, nil
}

// sealEntry serializes and encrypts-to-self one ledger.Entry. A faithful A'
// pocket must round-trip the FULL ledger.Entry (its cashctl-assigned ID, the
// throwaway/redeem/created-by-call fields the NIP-CP 9525 cannot carry) so the
// in-memory ledger buffer reloads byte-identical — the pure-NIP-CP 9525 in
// pocket.db is the interop/sync copy; this sealed blob is cmd's source of truth.
func sealEntry(e ledger.Entry, priv, pub string) (string, error) {
	blob, err := gobEncode(e)
	if err != nil {
		return "", fmt.Errorf("pocket: seal entry: %w", err)
	}
	sealed, err := nip44.EncryptHex(blob, priv, pub)
	if err != nil {
		return "", fmt.Errorf("pocket: seal entry: %w", err)
	}
	return sealed, nil
}

// openEntry decrypts and deserializes one ledger.Entry.
func openEntry(sealed, priv, pub string) (ledger.Entry, error) {
	blob, err := nip44.DecryptHex(sealed, priv, pub)
	if err != nil {
		return ledger.Entry{}, fmt.Errorf("pocket: open entry: %w", err)
	}
	var e ledger.Entry
	if err := gobDecode(blob, &e); err != nil {
		return ledger.Entry{}, fmt.Errorf("pocket: open entry: %w", err)
	}
	return e, nil
}

// sealHandoff serializes and encrypts-to-self one ledger.Handoff.
func sealHandoff(h ledger.Handoff, priv, pub string) (string, error) {
	blob, err := gobEncode(h)
	if err != nil {
		return "", fmt.Errorf("pocket: seal handoff: %w", err)
	}
	sealed, err := nip44.EncryptHex(blob, priv, pub)
	if err != nil {
		return "", fmt.Errorf("pocket: seal handoff: %w", err)
	}
	return sealed, nil
}

// openHandoff decrypts and deserializes one ledger.Handoff.
func openHandoff(sealed, priv, pub string) (ledger.Handoff, error) {
	blob, err := nip44.DecryptHex(sealed, priv, pub)
	if err != nil {
		return ledger.Handoff{}, fmt.Errorf("pocket: open handoff: %w", err)
	}
	var h ledger.Handoff
	if err := gobDecode(blob, &h); err != nil {
		return ledger.Handoff{}, fmt.Errorf("pocket: open handoff: %w", err)
	}
	return h, nil
}

// gobEncode gob-encodes v to a base64 string (gob bytes are not valid UTF-8, so
// they are base64'd before being handed to nip44 as a string plaintext).
func gobEncode(v any) (string, error) {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(v); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// gobDecode reverses gobEncode.
func gobDecode(s string, v any) error {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return err
	}
	return gob.NewDecoder(bytes.NewReader(raw)).Decode(v)
}
