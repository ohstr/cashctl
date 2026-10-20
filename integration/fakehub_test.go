//go:build integration

// fakehub_test.go is a relay that also plays a Cash Hub, for driving the
// binary against a Hub that lies. Ported from nmilat's own test hub
// (nipcash/client/batch_e2e_test.go): it serves a signed private-transport
// announcement, decrypts each kind-23190 request with its inbox key, and
// answers under the derived reply key, so everything cashctl does on the
// wire is the real code path. Tokens it mints carry a mint signature by the
// hub's own key, which is all a client checks before trusting its
// announcement, so the binary treats it exactly like a real minter.
package integration

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/gorilla/websocket"

	"github.com/ohstr/nmilat/nip01"
	"github.com/ohstr/nmilat/nip44"
	"github.com/ohstr/nmilat/nipcash"
	"github.com/ohstr/nmilat/nipcash/transport"
	"github.com/ohstr/nmilat/utils"
	"github.com/ohstr/nmilat/wire"
)

type fakeHub struct {
	t                     *testing.T
	minter                *btcec.PrivateKey
	hubPriv, hubXOnly     string
	inboxPriv, inboxXOnly string
	limits                transport.Limits
	url                   string

	// decide answers one item; nil omits it, as a real hub does for an item
	// it will not serve.
	decide func(item transport.Item) *transport.Result
	// lieTotal, when > 0, is the chunk total claimed by every reply to an
	// envelope carrying a lieTotalFor item, so a reply can say more chunks
	// are coming than ever will.
	lieTotal    int
	lieTotalFor string
	// silent drops every reply.
	silent bool
	// announcements, when set, replaces the hub's own signed announcement.
	announcements func() []*nip01.Event

	mu       sync.Mutex
	subs     []chan *nip01.Event
	requests []*nip01.Event
	items    []transport.Item
}

// newFakeHub starts a fake hub answering items with decide.
func newFakeHub(t *testing.T, decide func(transport.Item) *transport.Result) *fakeHub {
	t.Helper()
	minter, err := btcec.NewPrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	hubPriv := hex.EncodeToString(minter.Serialize())
	hubXOnly, err := utils.GetPublicKey(hubPriv)
	if err != nil {
		t.Fatal(err)
	}
	inboxPriv := randomHex32(t)
	inboxXOnly, err := utils.GetPublicKey(inboxPriv)
	if err != nil {
		t.Fatal(err)
	}
	h := &fakeHub{
		t: t, minter: minter, hubPriv: hubPriv, hubXOnly: hubXOnly,
		inboxPriv: inboxPriv, inboxXOnly: inboxXOnly,
		limits: transport.DefaultLimits(), decide: decide,
	}
	h.url = h.serve()
	return h
}

// signedAnnouncement is the hub's announcement naming inbox, signed by priv
// on behalf of node.
func signedAnnouncement(t *testing.T, priv, node, inbox string, limits transport.Limits, createdAt uint64) *nip01.Event {
	t.Helper()
	ev, err := transport.NewAnnouncement(node, inbox, limits, nil)
	if err != nil {
		t.Fatal(err)
	}
	if createdAt != 0 {
		ev.CreatedAt = createdAt
	}
	if err := ev.Sign(priv); err != nil {
		t.Fatal(err)
	}
	return ev
}

// mint returns a bill of amountMillis this hub "minted": a fresh wallet
// key, a mint signature by the hub's key, and the hub's relay. cashSecret,
// when non-empty, makes it a cash-mode bill handed out as token#cashSecret.
func (h *fakeHub) mint(amountMillis uint64, cashSecret string) (bill, walletPubkey string) {
	h.t.Helper()
	walletPubkey = fakeHex32(h.t)
	payload := nipcash.MintPayload("lokicash", walletPubkey, amountMillis)
	first := sha256.Sum256([]byte(nipcash.LNSignedMessagePrefix + payload))
	second := sha256.Sum256(first[:])
	tok, err := nipcash.Encode(nipcash.Token{
		HRP:                  "lokicash",
		WalletPubkey:         walletPubkey,
		Secret:               fakeHex32(h.t),
		RelayURLs:            []string{h.url},
		MintSignature:        ecdsa.SignCompact(h.minter, second[:], true),
		AttestedAmountMillis: &amountMillis,
	})
	if err != nil {
		h.t.Fatalf("encode fake hub bill: %v", err)
	}
	if cashSecret != "" {
		return tok + "#" + cashSecret, walletPubkey
	}
	return tok, walletPubkey
}

// itemsFor returns every item this hub received for method.
func (h *fakeHub) itemsFor(method string) []transport.Item {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []transport.Item
	for _, it := range h.items {
		if it.Method == method {
			out = append(out, it)
		}
	}
	return out
}

// requestsTo returns every kind-23190 request p-tagged to inbox.
func (h *fakeHub) requestsTo(inbox string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, ev := range h.requests {
		for _, tag := range ev.Tags {
			if len(tag) >= 2 && tag[0] == "p" && tag[1] == inbox {
				n++
			}
		}
	}
	return n
}

func (h *fakeHub) broadcast(ev *nip01.Event) {
	h.mu.Lock()
	subs := append([]chan *nip01.Event(nil), h.subs...)
	h.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *fakeHub) handleRequest(ev *nip01.Event) {
	h.mu.Lock()
	h.requests = append(h.requests, ev)
	h.mu.Unlock()

	conversationKey, err := transport.ConversationKeyFor(ev.PubKey, h.inboxPriv)
	if err != nil {
		return
	}
	plaintext, err := nip44.Decrypt(ev.Content, conversationKey[:])
	if err != nil {
		return // not for our inbox
	}
	env, err := transport.Decode([]byte(plaintext), h.limits)
	if err != nil {
		h.t.Errorf("fake hub: decode envelope: %v", err)
		return
	}
	h.mu.Lock()
	h.items = append(h.items, env.Items...)
	h.mu.Unlock()
	if h.silent {
		return
	}

	resp := transport.ResponseEnvelope{Version: transport.EnvelopeVersion, ReqNonce: env.Nonce, Seq: 1, Total: 1}
	for _, item := range env.Items {
		if r := h.decide(item); r != nil {
			resp.Results = append(resp.Results, *r)
		}
	}
	if h.lieTotal > 0 {
		for _, item := range env.Items {
			if item.Method == h.lieTotalFor {
				resp.Total = h.lieTotal
			}
		}
	}
	replyKey, err := transport.DeriveReplyKey(conversationKey, env.ReplyTo)
	if err != nil {
		h.t.Errorf("fake hub: reply key: %v", err)
		return
	}
	encoded, err := resp.EncodeResponse(h.limits)
	if err != nil {
		h.t.Errorf("fake hub: encode response: %v", err)
		return
	}
	sealed, err := nip44.Encrypt(string(encoded), replyKey[:])
	if err != nil {
		h.t.Errorf("fake hub: encrypt response: %v", err)
		return
	}
	out := nip01.NewEvent(transport.KindPrivateResponse, sealed)
	out.Tags = [][]string{{"p", env.ReplyTo}}
	if err := out.Sign(h.hubPriv); err != nil {
		h.t.Errorf("fake hub: sign response: %v", err)
		return
	}
	h.broadcast(out)
}

// serve runs the relay and returns its ws:// URL.
func (h *fakeHub) serve() string {
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var writeMu sync.Mutex
		write := func(v json.Marshaler) bool {
			b, err := v.MarshalJSON()
			if err != nil {
				return false
			}
			writeMu.Lock()
			defer writeMu.Unlock()
			return conn.WriteMessage(websocket.TextMessage, b) == nil
		}
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var payload wire.RelayPayload
			if err := json.Unmarshal(data, &payload); err != nil {
				continue
			}
			switch p := payload.Packet.(type) {
			case *wire.RequestPacket:
				subID := p.SubscriptionID
				announcement := false
				for _, f := range p.Filters.GetAll() {
					for _, k := range f.Kinds {
						if k == transport.KindHubAnnouncement {
							announcement = true
						}
					}
				}
				if announcement {
					anns := []*nip01.Event{signedAnnouncement(h.t, h.hubPriv, h.hubXOnly, h.inboxXOnly, h.limits, 0)}
					if h.announcements != nil {
						anns = h.announcements()
					}
					for _, ann := range anns {
						b, err := json.Marshal(ann)
						if err != nil {
							return
						}
						write(&wire.EventSubscriptionResponse{SubscriptionID: subID, EventBytes: b})
					}
					write(&wire.EOSESubscriptionResponse{SubscriptionID: subID})
					continue
				}
				ch := make(chan *nip01.Event, 16)
				h.mu.Lock()
				h.subs = append(h.subs, ch)
				h.mu.Unlock()
				write(&wire.EOSESubscriptionResponse{SubscriptionID: subID})
				go func() {
					for ev := range ch {
						b, err := json.Marshal(ev)
						if err != nil {
							return
						}
						if !write(&wire.EventSubscriptionResponse{SubscriptionID: subID, EventBytes: b}) {
							return
						}
					}
				}()
			case *wire.EventPacket:
				write(&wire.OkSubscriptionResponse{EventID: p.Event.ID, Accepted: true})
				if p.Event.Kind == transport.KindPrivateRequest {
					go h.handleRequest(p.Event)
				}
			}
		}
	})
	server := httptest.NewServer(mux)
	h.t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http")
}

// statusReply answers a cash_status item with one unclaimed, fee-free slice.
func statusReply(item transport.Item, identityType, identityValue string, amountMillis uint64) *transport.Result {
	return quoteReply(item, identityType, identityValue, amountMillis, 0)
}

// quoteReply is statusReply with a redeem fee.
func quoteReply(item transport.Item, identityType, identityValue string, amountMillis, feeMillis uint64) *transport.Result {
	row := map[string]any{
		"identity_type": identityType, "amount_millis": amountMillis, "claimed": false,
		"redeem_fee_millis": feeMillis, "net_redeemable_millis": amountMillis - feeMillis,
	}
	if identityValue != "" {
		row["identity_value"] = identityValue
	}
	b, _ := json.Marshal(map[string]any{"recipients": []any{row}})
	return &transport.Result{ID: item.ID, ResultType: item.Method, Result: b}
}

// TestFakeHub_HonestReceive is the harness's own check: an honest answer
// from the fake hub is received like a real Hub's.
func TestFakeHub_HonestReceive(t *testing.T) {
	f, pub := newMember(t)
	hub := newFakeHub(t, func(item transport.Item) *transport.Result {
		return statusReply(item, "pubkey", pub, 25_000)
	})
	bill, _ := hub.mint(25_000, "")
	out := requireOK(t, "receive from the fake hub", f.run("receive", bill))
	entry, _ := out["entry"].(map[string]any)
	if got, _ := entry["amount_millis"].(float64); got != 25_000 {
		t.Errorf("received amount = %v, want 25000: %v", entry["amount_millis"], out)
	}
	if n := len(hub.itemsFor("cash_status")); n == 0 {
		t.Error("the fake hub saw no cash_status: receive did not go over the private transport")
	}
}
