package cmd

// Adversarial ATTACK sweep on `cash pocket serve` (NIP-CP, kind-1059 gift-wraps).
// `serve` fronts the local pocket.db as a NIP-01 relay so a SECOND device on the
// SAME key can sync off it. pocketServeHandler gates it with
// relay.WithRecipientOnlyKinds(1059): a 1059 is served only to a connection that
// has NIP-42-authenticated as a pubkey named in the event's `p` tags.
//
// These tests drive the real handler over a loopback WebSocket with two keys
// (owner + attacker) and prove, as TRUE-NEGATIVES (each FAILS if the gate were
// dropped):
//   1. a NON-owner key pulls NONE of the owner's wraps (per-event `p` gate);
//   4. an UN-authenticated REQ naming 1059 is CLOSED auth-required, and neither an
//      un-authed nor a foreign-authed kind-less REQ leaks anything;
//   +  the owner (same key) DOES get its wrap — the gate is not deny-all; and
//   3. a wrap a foreign client INJECTS into the serve is inert: the owner may pull
//      it (it is `p`-tagged to them) but nipCP.OpenAll/ImportHeld discard it, so it
//      never becomes spendable (the serve ingress twin of CC-57/CC-58).
// Concern 2 (pulled wraps unreadable by a foreign key) is the import layer, proven
// by internal/pocket CC-57; here the foreign key pulls nothing to begin with.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip42"
	"github.com/ohstr/nmilat/nipCP"
	"github.com/ohstr/nmilat/utils"

	"github.com/ohstr/cashctl/internal/ledger"
	"github.com/ohstr/cashctl/internal/pocket"
)

// serveWS is a minimal raw NIP-01/NIP-42 WebSocket client for driving a served
// pocket: it remembers the AUTH challenge the relay opens with.
type serveWS struct {
	t         *testing.T
	conn      *websocket.Conn
	url       string
	challenge string
}

func dialServe(t *testing.T, url string) *serveWS {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := &serveWS{t: t, conn: conn, url: url}
	label, raw := c.read()
	if label != "AUTH" {
		t.Fatalf("first server message = %s, want an AUTH challenge", label)
	}
	_ = json.Unmarshal(raw[1], &c.challenge)
	return c
}

func (c *serveWS) read() (string, []json.RawMessage) {
	c.t.Helper()
	_ = c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var raw []json.RawMessage
	if err := c.conn.ReadJSON(&raw); err != nil {
		c.t.Fatalf("read: %v", err)
	}
	var label string
	_ = json.Unmarshal(raw[0], &label)
	return label, raw
}

func (c *serveWS) until(labels ...string) (string, []json.RawMessage) {
	c.t.Helper()
	for {
		label, raw := c.read()
		for _, l := range labels {
			if label == l {
				return label, raw
			}
		}
	}
}

func (c *serveWS) send(msg ...any) {
	c.t.Helper()
	if err := c.conn.WriteJSON(msg); err != nil {
		c.t.Fatalf("write: %v", err)
	}
}

func (c *serveWS) auth(t *testing.T, priv string) {
	t.Helper()
	ev := nip42.NewAuthEvent(c.challenge, c.url)
	if err := ev.Sign(priv); err != nil {
		t.Fatal(err)
	}
	c.send("AUTH", ev)
	_, raw := c.until("OK")
	var ok bool
	_ = json.Unmarshal(raw[2], &ok)
	if !ok {
		t.Fatalf("AUTH refused: %s", raw[3])
	}
}

// req runs one REQ and returns the delivered event ids before EOSE, or the
// CLOSED reason if the subscription was refused.
func (c *serveWS) req(subID string, filter map[string]any) (ids []string, closed string) {
	c.t.Helper()
	c.send("REQ", subID, filter)
	for {
		label, raw := c.until("EVENT", "EOSE", "CLOSED")
		switch label {
		case "EVENT":
			var ev nip01.Event
			_ = json.Unmarshal(raw[2], &ev)
			ids = append(ids, ev.ID)
		case "EOSE":
			return ids, ""
		case "CLOSED":
			_ = json.Unmarshal(raw[2], &closed)
			return ids, closed
		}
	}
}

func (c *serveWS) publish(ev *nip01.Event) (ok bool, reason string) {
	c.t.Helper()
	c.send("EVENT", ev)
	_, raw := c.until("OK")
	_ = json.Unmarshal(raw[2], &ok)
	if len(raw) > 3 {
		_ = json.Unmarshal(raw[3], &reason)
	}
	return ok, reason
}

// startServe projects one held bill (owner = testPocketKey) into a pocket and
// fronts it with the real pocketServeHandler over an ephemeral loopback socket.
func startServe(t *testing.T) (url, ownerPub string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	ownerPub, err := utils.GetPublicKey(testPocketKey)
	if err != nil {
		t.Fatal(err)
	}
	held, _ := heldBill(t, 40_000)
	p, err := pocket.Open(ctx, t.TempDir(), testPocketKey, "flokicoin")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url = "ws://" + ln.Addr().String()
	handler, err := pocketServeHandler(ctx, p, []ledger.Entry{held}, url)
	if err != nil {
		_ = ln.Close()
		t.Fatal(err)
	}
	t.Cleanup(handler.VerificationWorker.Stop)
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return url, ownerPub
}

// Concerns 1 + 4 + positive control: a foreign key pulls nothing, an un-authed
// REQ naming 1059 is CLOSED auth-required, kind-less probes leak nothing, and the
// owner (same key) gets its wrap. Each assertion FAILS if WithRecipientOnlyKinds
// were dropped from pocketServeHandler.
func TestServeAttack_NonOwnerPullsNothing(t *testing.T) {
	url, ownerPub := startServe(t)
	const attackerPriv = "2222222222222222222222222222222222222222222222222222222222222222"

	// Un-authenticated REQ naming kind 1059 → refused auth-required (concern 4),
	// so an eavesdropper cannot even blind-pull.
	anon := dialServe(t, url)
	if ids, closed := anon.req("a", map[string]any{"kinds": []int{1059}}); len(ids) != 0 || !strings.HasPrefix(closed, "auth-required:") {
		t.Fatalf("FINDING: un-authed REQ for 1059 not auth-gated: ids=%v closed=%q", ids, closed)
	}
	// Un-authed kind-LESS REQ: not auth-required (names no gated kind) but the
	// per-event gate still withholds the owner's wraps — the store holds only
	// 1059s, all p-tagged to the owner, so nothing comes back.
	if ids, closed := anon.req("b", map[string]any{"#p": []string{ownerPub}}); closed != "" || len(ids) != 0 {
		t.Fatalf("FINDING: un-authed kind-less REQ leaked wraps: ids=%v closed=%q", ids, closed)
	}

	// Foreign key, authenticated as ITSELF: it clears NIP-42 but is named in no
	// wrap's p tag, so the per-event gate serves it nothing (concern 1).
	att := dialServe(t, url)
	att.auth(t, attackerPriv)
	if ids, closed := att.req("a", map[string]any{"kinds": []int{1059}}); closed != "" || len(ids) != 0 {
		t.Fatalf("FINDING: an authed NON-owner pulled the owner's 1059 wraps: ids=%v closed=%q", ids, closed)
	}
	// Even asking directly for the owner's #p (what a thief would try) yields nothing.
	if ids, _ := att.req("c", map[string]any{"kinds": []int{1059}, "#p": []string{ownerPub}}); len(ids) != 0 {
		t.Fatalf("FINDING: a NON-owner pulled the owner's wraps by naming the owner's #p: %v", ids)
	}
	// A kind-less filter from the foreign key: still nothing (no other kinds exist,
	// and the 1059s are withheld) — the serve leaks no metadata.
	if ids, closed := att.req("d", map[string]any{}); closed != "" || len(ids) != 0 {
		t.Fatalf("FINDING: a NON-owner's kind-less REQ leaked events: ids=%v closed=%q", ids, closed)
	}

	// Positive control: the OWNER (same key) authenticates and DOES get its wrap —
	// the gate is recipient-scoped, not deny-all.
	me := dialServe(t, url)
	me.auth(t, testPocketKey)
	if ids, closed := me.req("a", map[string]any{"kinds": []int{1059}, "#p": []string{ownerPub}}); closed != "" || len(ids) != 1 {
		t.Fatalf("the owner could not pull its own wrap: ids=%v closed=%q", ids, closed)
	}
}

// Concern 3 — WRITE / INJECT: a foreign client publishes a kind-1059 `p`-tagged to
// the owner into the serve. The owner may pull it (it names them), but it was not
// sealed encrypt-to-self with the owner's key, so nipCP.OpenAll discards it and
// ImportHeld makes nothing spendable. Ingress injection is inert.
func TestServeAttack_InjectedWrapIsInert(t *testing.T) {
	url, ownerPub := startServe(t)
	const attackerPriv = "2222222222222222222222222222222222222222222222222222222222222222"

	// Attacker crafts a kind-1059 addressed to the owner (so it clears the read
	// gate) but signed by its own ephemeral key with content the owner cannot open.
	injected, err := nip01.NewSignedEvent(1059, "not a wrap the owner can open", attackerPriv, []string{"p", ownerPub})
	if err != nil {
		t.Fatal(err)
	}
	att := dialServe(t, url)
	att.auth(t, attackerPriv)
	if ok, reason := att.publish(injected); !ok {
		// If the serve rejects the publish outright, injection is even more inert —
		// record it and stop (still DEFENDED).
		t.Logf("serve rejected the injected 1059 at ingress: %s", reason)
		return
	}

	// Owner pulls kind 1059: it DOES receive the injected event (p = owner), plus
	// its own real wrap.
	me := dialServe(t, url)
	me.auth(t, testPocketKey)
	ids, closed := me.req("a", map[string]any{"kinds": []int{1059}, "#p": []string{ownerPub}})
	if closed != "" {
		t.Fatalf("owner REQ closed: %q", closed)
	}
	sawInjected := false
	for _, id := range ids {
		if id == injected.ID {
			sawInjected = true
		}
	}
	if !sawInjected {
		t.Skip("serve withheld the injected p=owner event from the owner — even more defended; nothing to prove inert")
	}

	// Decisive: run the pulled events through the real import pipeline. OpenAll
	// keeps only wraps sealed by the owner to the owner; the injected one is
	// discarded, so it never becomes an entry — let alone spendable.
	pulled := me.rawEvents(t, "p2", map[string]any{"kinds": []int{1059}, "#p": []string{ownerPub}})
	opened, err := nipCP.OpenAll(pulled, testPocketKey)
	if err != nil {
		t.Fatalf("OpenAll: %v", err)
	}
	// None of the opened entries may originate from the injected event. Prove it by
	// importing into a fresh pocket and confirming the injected event contributed
	// nothing beyond the owner's own single real bill.
	ctx := context.Background()
	pImp, err := pocket.Open(ctx, t.TempDir(), testPocketKey, "flokicoin")
	if err != nil {
		t.Fatal(err)
	}
	defer pImp.Close()
	spendable, err := pImp.ImportHeld(ctx, opened)
	if err != nil {
		t.Fatalf("ImportHeld: %v", err)
	}
	if spendable > 1 {
		t.Fatalf("FINDING: an injected foreign 1059 produced extra spendable bills (%d) — ingress injection not inert", spendable)
	}
	if n := len(opened); n > 1 {
		t.Fatalf("FINDING: the injected 1059 opened as a user entry (%d entries) — OpenAll must discard a foreign-sealed wrap", n)
	}
}

// rawEvents re-runs a REQ and returns the delivered events (not just ids).
func (c *serveWS) rawEvents(t *testing.T, subID string, filter map[string]any) []*nip01.Event {
	t.Helper()
	c.send("REQ", subID, filter)
	var out []*nip01.Event
	for {
		label, raw := c.until("EVENT", "EOSE", "CLOSED")
		switch label {
		case "EVENT":
			var ev nip01.Event
			_ = json.Unmarshal(raw[2], &ev)
			cp := ev
			out = append(out, &cp)
		case "EOSE", "CLOSED":
			return out
		}
	}
}
