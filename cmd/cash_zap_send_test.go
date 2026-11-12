package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nipCZ"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/appdir"
	"github.com/ohstr/cashctl/internal/identity"
	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/testsupport/testmint"
)

// zapSendEnv is a local identity + an isolated appdir, the minimum for the
// zap-send helpers (no Hub, no network).
func zapSendEnv(t *testing.T) (me string) {
	t.Helper()
	appdir.SetOverride(t.TempDir())
	t.Cleanup(func() { appdir.SetOverride("") })
	if _, err := identity.GenerateAndSaveLocal(); err != nil {
		t.Fatal(err)
	}
	priv, err := identity.Resolve(nil)
	if err != nil {
		t.Fatal(err)
	}
	me, _ = utils.GetPublicKey(priv)
	return me
}

func rcptPubkey(t *testing.T) nipCZ.Identity {
	t.Helper()
	_, pub := testmint.Key(t)
	return nipCZ.Identity{Value: pub}
}

func rcptConnKey(t *testing.T) nipCZ.Identity {
	t.Helper()
	return nipCZ.Identity{Value: testmint.Hex32(t), Platform: "discord"}
}

// TestZapRecipient covers recipient parsing: a hex pubkey, a connection key
// (--platform + --ia), and the refusals.
func TestZapRecipient(t *testing.T) {
	_, pub := testmint.Key(t)
	_, iaPub := testmint.Key(t)

	t.Run("pubkey hex", func(t *testing.T) {
		c := zapCmd(newCashZapSendCmd(), nil)
		id, err := zapRecipient(c, pub)
		if err != nil {
			t.Fatal(err)
		}
		if id.Value != pub || id.IsConnectionKey() {
			t.Fatalf("got %+v", id)
		}
	})

	t.Run("connection key", func(t *testing.T) {
		ck := testmint.Hex32(t)
		c := zapCmd(newCashZapSendCmd(), map[string]string{"platform": "discord", "ia": iaPub})
		id, err := zapRecipient(c, ck)
		if err != nil {
			t.Fatal(err)
		}
		if !id.IsConnectionKey() || id.Platform != "discord" || id.Value != ck {
			t.Fatalf("got %+v", id)
		}
	})

	t.Run("connection key without ia is refused", func(t *testing.T) {
		ck := testmint.Hex32(t)
		c := zapCmd(newCashZapSendCmd(), map[string]string{"platform": "discord"})
		if _, err := zapRecipient(c, ck); err == nil {
			t.Fatal("expected an error without --ia")
		}
	})

	t.Run("garbage recipient is refused", func(t *testing.T) {
		c := zapCmd(newCashZapSendCmd(), nil)
		if _, err := zapRecipient(c, "not-a-pubkey"); err == nil {
			t.Fatal("expected an error")
		}
	})
}

// TestZapTargetEvent covers the --event target: a valid signed event is read, a
// tampered one is refused (rule 7 trusts the tags only after the signature), and
// a connection-key recipient with an event is refused.
func TestZapTargetEvent(t *testing.T) {
	priv, _ := testmint.Key(t)
	note, err := nip01.NewSignedEvent(1, "a note", priv)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(note)
	path := filepath.Join(t.TempDir(), "note.json")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("valid event", func(t *testing.T) {
		c := zapCmd(newCashZapSendCmd(), map[string]string{"event": path})
		ev, err := zapTargetEvent(c, rcptPubkey(t))
		if err != nil {
			t.Fatal(err)
		}
		if ev == nil || ev.ID != note.ID {
			t.Fatalf("got %+v", ev)
		}
	})

	t.Run("tampered event is refused", func(t *testing.T) {
		bad := *note
		bad.Content = "changed"
		bb, _ := json.Marshal(bad)
		badPath := filepath.Join(t.TempDir(), "bad.json")
		_ = os.WriteFile(badPath, bb, 0o600)
		c := zapCmd(newCashZapSendCmd(), map[string]string{"event": badPath})
		if _, err := zapTargetEvent(c, rcptPubkey(t)); err == nil {
			t.Fatal("expected a signature error")
		}
	})

	t.Run("connection-key recipient with an event is refused", func(t *testing.T) {
		c := zapCmd(newCashZapSendCmd(), map[string]string{"event": path})
		if _, err := zapTargetEvent(c, rcptConnKey(t)); err == nil {
			t.Fatal("expected an error for a connection-key event")
		}
	})
}

// TestSaveZapResult covers the funds-save discipline and the countable matrix
// with a synthetic transfer reply (no Hub): the source is marked transferred,
// the recipient's token is kept as a hand-off, and `countable` is only true for
// a pubkey zap on an event whose reply was zap-signed and that a relay accepted.
func TestSaveZapResult(t *testing.T) {
	me := zapSendEnv(t)
	amount := uint64(21000)
	iss, exp := uint64(1760000000), uint64(1790000000)
	// In-place full-amount reply: RecipientToken falls back to the source token.
	result := &nipcash.CashTransferResult{AmountMillis: amount, IssuedAt: &iss, ExpiresAt: &exp}

	signer, _ := testmint.Key(t)
	zapEv, err := nip01.NewSignedEvent(nipCZ.KindCashZap, "gm", signer)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name          string
		recipient     nipCZ.Identity
		target        bool
		accepted      []string
		wantCountable bool
	}{
		{"countable pubkey on event", rcptPubkey(t), true, []string{"wss://r.example"}, true},
		{"no event is not counted", rcptPubkey(t), false, []string{"wss://r.example"}, false},
		{"connection key is not counted", rcptConnKey(t), false, []string{"wss://r.example"}, false},
		{"no relay accepted is not counted", rcptPubkey(t), true, nil, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			l, err := ledger.Load()
			if err != nil {
				t.Fatal(err)
			}
			src, err := l.Add(ledger.Entry{Token: fmt.Sprintf("satscash1src%d", i), WalletPubkey: fmt.Sprintf("w%d", i),
				IdentityPubkey: me, IdentityRequired: ptrTo(true), AmountMillis: &amount})
			if err != nil {
				t.Fatal(err)
			}
			// runCashZapSend marks the source transferred before saveZapResult;
			// mirror that and assert saveZapResult preserves it.
			_ = l.SetStatus(src.ID, ledger.StatusTransferred)
			var tgt *nip01.Event
			if c.target {
				tgt = zapEv // any signed event; saveZapResult only checks nil-ness
			}
			cmd := zapCmd(newCashZapSendCmd(), nil)
			out, _ := captureStreams(t, func() {
				if err := saveZapResult(cmd, l, result, nil, src, amount, c.recipient, zapEv, nil, tgt, c.accepted, nil); err != nil {
					t.Fatalf("saveZapResult: %v", err)
				}
			})
			var doc map[string]any
			if err := json.Unmarshal([]byte(out), &doc); err != nil {
				t.Fatalf("%q: %v", out, err)
			}
			if doc["countable"] != c.wantCountable {
				t.Errorf("countable = %v, want %v (%v)", doc["countable"], c.wantCountable, doc)
			}
			if doc["zapped"] != true {
				t.Errorf("not reported zapped: %v", doc)
			}
			if e, ok := l.Find(src.ID); !ok || e.Status != ledger.StatusTransferred {
				t.Errorf("source not marked transferred: %+v", e)
			}
		})
	}

	t.Run("zap build failure keeps the value as a transfer", func(t *testing.T) {
		l, err := ledger.Load()
		if err != nil {
			t.Fatal(err)
		}
		src, _ := l.Add(ledger.Entry{Token: "satscash1sourcebill2", WalletPubkey: "w2", IdentityPubkey: me,
			IdentityRequired: ptrTo(true), AmountMillis: &amount})
		cmd := zapCmd(newCashZapSendCmd(), nil)
		out, _ := captureStreams(t, func() {
			if err := saveZapResult(cmd, l, result, nil, src, amount, rcptPubkey(t), nil,
				nipCZ.ErrZapNotSigned, zapEv, nil, nil); err != nil {
				t.Fatalf("saveZapResult: %v", err)
			}
		})
		var doc map[string]any
		_ = json.Unmarshal([]byte(out), &doc)
		if doc["countable"] != false || doc["zap_error"] == nil {
			t.Errorf("a zap build failure should be non-countable with a zap_error: %v", doc)
		}
		if doc["recipient_token"] == nil {
			t.Errorf("the recipient's token must still be reported so the value is recoverable: %v", doc)
		}
	})
}
