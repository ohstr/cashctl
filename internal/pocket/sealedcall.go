package pocket

import (
	"bytes"
	"encoding/base64"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/ohstr/nmilat/nip44"

	"github.com/ohstr/cashctl/internal/ledger"
)

// A cashctl ledger.Call/Entry/Handoff carries secrets (AuthorizerSecret,
// Secrets-by-role, CashSecret, ThrowawayKey) and some fields in json:"-" fields,
// so it cannot be JSON-marshalled whole. These helpers serialize the FULL value
// with gob (every exported field) and NIP-44 encrypt-to-self, so the in-flight
// call/held bill is stored encrypted at rest in pocket-cache.db (the plan's risk
// 17) and loaded back intact.
//
// gob gotcha (adversarial finding #16, and a confirmed funds-state bug it caused):
// gob OMITS pointers to a zero value — a *bool(&false) or *uint64(&0) round-trips
// to nil. ledger.Entry.IdentityRequired is *bool; a cash note with
// IdentityRequired=&false lost its note-ness on reload, so a NOT_FOUND transfer
// failed to reclaim its source (TestSettle_TransferNotFoundIsReclaimed). So the
// gob blob is wrapped with a JSON "pointer overlay" that preserves nil vs
// pointer-to-zero for every pointer field, re-applied after gob-decode; and every
// seal does a seal→open reflect.DeepEqual read-back (fail-closed) so ANY field
// that does not round-trip errors loudly instead of silently corrupting state.

// sealedBlob is the wire form: a gob-encoded struct plus a JSON pointer overlay.
type sealedBlob struct {
	Gob     string          `json:"g"`           // base64(gob) of the full struct
	Pointer json.RawMessage `json:"p,omitempty"` // JSON of the pointer-field overlay
}

// entryPtrs / callPtrs / handoffPtrs mirror every pointer field of their type.
// No omitempty: a nil pointer encodes as null (→ nil), a pointer-to-zero as its
// value (→ pointer-to-zero) — the distinction gob loses.
type entryPtrs struct {
	IdentityRequired       *bool   `json:"identity_required"`
	AmountMillis           *uint64 `json:"amount_millis"`
	MinterPubkey           *string `json:"minter_pubkey"`
	ExpiresAt              *int64  `json:"expires_at"`
	RedeemFeeMillis        *uint64 `json:"redeem_fee_millis"`
	RedeemInvoiceExpiresAt *int64  `json:"redeem_invoice_expires_at"`
	RedeemPresumedAt       *int64  `json:"redeem_presumed_at"`
}

type callPtrs struct {
	RetryAfter *int64 `json:"retry_after"`
}

type handoffPtrs struct {
	ExpiresAt *int64 `json:"expires_at"`
}

func entryPtrsOf(e ledger.Entry) entryPtrs {
	return entryPtrs{e.IdentityRequired, e.AmountMillis, e.MinterPubkey, e.ExpiresAt, e.RedeemFeeMillis, e.RedeemInvoiceExpiresAt, e.RedeemPresumedAt}
}
func (p entryPtrs) applyTo(e *ledger.Entry) {
	e.IdentityRequired, e.AmountMillis, e.MinterPubkey, e.ExpiresAt, e.RedeemFeeMillis, e.RedeemInvoiceExpiresAt, e.RedeemPresumedAt = p.IdentityRequired, p.AmountMillis, p.MinterPubkey, p.ExpiresAt, p.RedeemFeeMillis, p.RedeemInvoiceExpiresAt, p.RedeemPresumedAt
}

// sealCall serializes and encrypts-to-self one ledger.Call.
func sealCall(c ledger.Call, priv, pub string) (string, error) {
	return seal("call", c, callPtrs{c.RetryAfter}, priv, pub, func(s string) (any, error) { return openCall(s, priv, pub) })
}

// openCall decrypts and deserializes one ledger.Call.
func openCall(sealed, priv, pub string) (ledger.Call, error) {
	var c ledger.Call
	if err := unseal(sealed, priv, pub, &c, func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var p callPtrs
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		c.RetryAfter = p.RetryAfter
		return nil
	}); err != nil {
		return ledger.Call{}, fmt.Errorf("pocket: open call: %w", err)
	}
	return c, nil
}

// sealEntry serializes and encrypts-to-self one ledger.Entry. A faithful A'
// pocket must round-trip the FULL ledger.Entry (its cashctl-assigned ID, the
// throwaway/redeem/created-by-call fields the NIP-CP 9525 cannot carry, and its
// pointer fields incl. IdentityRequired) so the in-memory ledger buffer reloads
// byte-identical — the pure-NIP-CP 9525 in pocket.db is the interop/sync copy;
// this sealed blob is cmd's source of truth.
func sealEntry(e ledger.Entry, priv, pub string) (string, error) {
	return seal("entry", e, entryPtrsOf(e), priv, pub, func(s string) (any, error) { return openEntry(s, priv, pub) })
}

// openEntry decrypts and deserializes one ledger.Entry.
func openEntry(sealed, priv, pub string) (ledger.Entry, error) {
	var e ledger.Entry
	if err := unseal(sealed, priv, pub, &e, func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var p entryPtrs
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		p.applyTo(&e)
		return nil
	}); err != nil {
		return ledger.Entry{}, fmt.Errorf("pocket: open entry: %w", err)
	}
	return e, nil
}

// sealHandoff serializes and encrypts-to-self one ledger.Handoff.
func sealHandoff(h ledger.Handoff, priv, pub string) (string, error) {
	return seal("handoff", h, handoffPtrs{h.ExpiresAt}, priv, pub, func(s string) (any, error) { return openHandoff(s, priv, pub) })
}

// openHandoff decrypts and deserializes one ledger.Handoff.
func openHandoff(sealed, priv, pub string) (ledger.Handoff, error) {
	var h ledger.Handoff
	if err := unseal(sealed, priv, pub, &h, func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var p handoffPtrs
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		h.ExpiresAt = p.ExpiresAt
		return nil
	}); err != nil {
		return ledger.Handoff{}, fmt.Errorf("pocket: open handoff: %w", err)
	}
	return h, nil
}

// orderPtrs / withdrawalPtrs are the overlay for ledger.Order/Withdrawal. Neither
// has pointer fields, but both carry an AcceptedRelays []string, and gob collapses
// an empty non-nil slice to nil on decode — so the slice is carried in the JSON
// overlay (which preserves nil vs [] vs non-empty) and re-applied after gob-decode,
// the same nil-vs-zero fix the entry/call overlays make. The seal→open DeepEqual
// read-back fails loudly if any other field does not round-trip.
type orderPtrs struct {
	AcceptedRelays []string `json:"accepted_relays"`
}
type withdrawalPtrs struct {
	AcceptedRelays []string `json:"accepted_relays"`
}

// sealOrder serializes and encrypts-to-self one ledger.Order (a NIP-CO order
// placed from here). Funds-bearing (its mint is collected against it), so stored
// encrypted at rest like calls/entries, byte-identical on reload.
func sealOrder(o ledger.Order, priv, pub string) (string, error) {
	return seal("order", o, orderPtrs{o.AcceptedRelays}, priv, pub, func(s string) (any, error) { return openOrder(s, priv, pub) })
}

// openOrder decrypts and deserializes one ledger.Order.
func openOrder(sealed, priv, pub string) (ledger.Order, error) {
	var o ledger.Order
	if err := unseal(sealed, priv, pub, &o, func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var p orderPtrs
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		o.AcceptedRelays = p.AcceptedRelays
		return nil
	}); err != nil {
		return ledger.Order{}, fmt.Errorf("pocket: open order: %w", err)
	}
	return o, nil
}

// sealWithdrawal serializes and encrypts-to-self one ledger.Withdrawal (an
// on-chain payout placed from here — funds-bearing, so encrypted at rest and
// byte-identical on reload).
func sealWithdrawal(w ledger.Withdrawal, priv, pub string) (string, error) {
	return seal("withdrawal", w, withdrawalPtrs{w.AcceptedRelays}, priv, pub, func(s string) (any, error) { return openWithdrawal(s, priv, pub) })
}

// openWithdrawal decrypts and deserializes one ledger.Withdrawal.
func openWithdrawal(sealed, priv, pub string) (ledger.Withdrawal, error) {
	var w ledger.Withdrawal
	if err := unseal(sealed, priv, pub, &w, func(raw json.RawMessage) error {
		if len(raw) == 0 {
			return nil
		}
		var p withdrawalPtrs
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		w.AcceptedRelays = p.AcceptedRelays
		return nil
	}); err != nil {
		return ledger.Withdrawal{}, fmt.Errorf("pocket: open withdrawal: %w", err)
	}
	return w, nil
}

// seal gob-encodes v + its pointer overlay, encrypts-to-self, and read-back
// verifies (seal→open reflect.DeepEqual) so any field that does not round-trip
// fails loudly here rather than silently corrupting funds state later. reopen
// decodes the sealed string back to the same concrete type for the DeepEqual.
func seal(kind string, v any, ptrs any, priv, pub string, reopen func(string) (any, error)) (string, error) {
	gobStr, err := gobEncode(v)
	if err != nil {
		return "", fmt.Errorf("pocket: seal %s: %w", kind, err)
	}
	ptrJSON, err := json.Marshal(ptrs)
	if err != nil {
		return "", fmt.Errorf("pocket: seal %s: %w", kind, err)
	}
	wire, err := json.Marshal(sealedBlob{Gob: gobStr, Pointer: ptrJSON})
	if err != nil {
		return "", fmt.Errorf("pocket: seal %s: %w", kind, err)
	}
	sealed, err := nip44.EncryptHex(string(wire), priv, pub)
	if err != nil {
		return "", fmt.Errorf("pocket: seal %s: %w", kind, err)
	}
	got, err := reopen(sealed)
	if err != nil {
		return "", fmt.Errorf("pocket: seal %s read-back: %w", kind, err)
	}
	if !reflect.DeepEqual(reflect.Indirect(reflect.ValueOf(got)).Interface(), v) {
		return "", fmt.Errorf("pocket: seal %s is not round-trippable (a field does not survive seal→open) — funds-state would be corrupted; fix the serialization/overlay", kind)
	}
	return sealed, nil
}

// unseal decrypts, gob-decodes into out, then applies the JSON pointer overlay.
func unseal(sealed, priv, pub string, out any, applyPtrs func(json.RawMessage) error) error {
	plain, err := nip44.DecryptHex(sealed, priv, pub)
	if err != nil {
		return err
	}
	var w sealedBlob
	if err := json.Unmarshal([]byte(plain), &w); err != nil {
		return err
	}
	if err := gobDecode(w.Gob, out); err != nil {
		return err
	}
	return applyPtrs(w.Pointer)
}

// gobEncode gob-encodes v to a base64 string (gob bytes are not valid UTF-8).
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
