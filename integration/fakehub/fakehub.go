// Package fakehub is a relay that also plays a Cash Hub, for driving cashctl
// against a Hub that lies. Ported from nmilat's own test hub
// (nipcash/client/batch_e2e_test.go): it serves a signed private-transport
// announcement, decrypts each kind-23190 request with its inbox key, and
// answers under the derived reply key, so everything cashctl does on the wire
// is the real code path. Bills it mints carry a mint signature by the hub's own
// key, which is all a client checks before trusting its announcement, so
// cashctl treats it exactly like a real minter.
//
// It is an ordinary package, not a _test.go file, so the offline integration
// tests and the agent-eval shim share one fake. The hostility knobs (LieTotal,
// DropReplies, DropRequests, AnswerCode, RandomMessages, SetAnnouncements) make
// it misbehave in one named way each.
package fakehub

import (
	"crypto/rand"
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

// Decider answers one item; nil omits it, as a real hub does for an item it
// will not serve.
type Decider func(item transport.Item) *transport.Result

// Hub is one fake Cash Hub and the relay it answers on.
type Hub struct {
	t                     testing.TB
	minter                *btcec.PrivateKey
	hubPriv, hubXOnly     string
	inboxPriv, inboxXOnly string
	limits                transport.Limits
	url                   string
	decide                Decider

	mu sync.Mutex
	// lieTotal, when > 0, is the chunk total claimed by every reply to an
	// envelope carrying a lieTotalFor item, so a reply can say more chunks
	// are coming than ever will.
	lieTotal    int
	lieTotalFor string
	// dropReplies counts, per method ("" for any), the replies still to drop;
	// a negative count drops every one.
	dropReplies map[string]int
	// dropRequests is how many requests are still to be ignored unread.
	dropRequests int
	// answerCode answers every item of a method with that error code.
	answerCode map[string]string
	// randomMessages replaces every error message with random text.
	randomMessages bool
	// announcements, when set, replaces the hub's own signed announcement.
	announcements func() []*nip01.Event

	subs     []chan *nip01.Event
	requests []*nip01.Event
	items    []transport.Item
}

// New starts a fake hub answering items with decide; it stops at the end of
// the test.
func New(t testing.TB, decide Decider) *Hub {
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
	inboxPriv := RandomHex32(t)
	inboxXOnly, err := utils.GetPublicKey(inboxPriv)
	if err != nil {
		t.Fatal(err)
	}
	h := &Hub{
		t: t, minter: minter, hubPriv: hubPriv, hubXOnly: hubXOnly,
		inboxPriv: inboxPriv, inboxXOnly: inboxXOnly,
		limits: transport.DefaultLimits(), decide: decide,
		dropReplies: map[string]int{}, answerCode: map[string]string{},
	}
	h.url = h.serve()
	return h
}

// URL is the relay's ws:// URL, named by every bill the hub mints.
func (h *Hub) URL() string { return h.url }

// Identity is the hub's node identity, x-only hex: what its announcement is for.
func (h *Hub) Identity() string { return h.hubXOnly }

// Inbox is the x-only pubkey its own announcement names for requests.
func (h *Hub) Inbox() string { return h.inboxXOnly }

// MinterPubkey is the compressed hex key the hub's bills are signed with.
func (h *Hub) MinterPubkey() string {
	return hex.EncodeToString(h.minter.PubKey().SerializeCompressed())
}

// Announcement is an announcement for this hub naming inbox, genuinely signed
// by the hub, with createdAt (0: now).
func (h *Hub) Announcement(inbox string, createdAt uint64) *nip01.Event {
	return SignedAnnouncement(h.t, h.hubPriv, h.hubXOnly, inbox, h.limits, createdAt)
}

// SetAnnouncements replaces the hub's own announcement with whatever fn
// returns at each request for one.
func (h *Hub) SetAnnouncements(fn func() []*nip01.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.announcements = fn
}

// LieTotal makes every reply to an envelope carrying a method item claim
// total chunks, while only ever one is sent.
func (h *Hub) LieTotal(method string, total int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.lieTotal, h.lieTotalFor = total, method
}

// DropReplies reads and records the next n envelopes carrying a method item
// ("" for any), then never answers them: the call reached the hub, its reply
// is lost. n < 0 drops every one.
func (h *Hub) DropReplies(method string, n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropReplies[method] = n
}

// DropRequests ignores the next n requests unread, as if they never arrived.
func (h *Hub) DropRequests(n int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropRequests = n
}

// AnswerCode answers every method item with error code, instead of asking
// the Decider.
func (h *Hub) AnswerCode(method, code string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.answerCode[method] = code
}

// RandomMessages replaces the message of every error the hub answers with
// random text, so a client acting on a message rather than its code is caught.
func (h *Hub) RandomMessages() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.randomMessages = true
}

// SignedAnnouncement is a hub announcement naming inbox, signed by priv on
// behalf of node, with createdAt (0: now).
func SignedAnnouncement(t testing.TB, priv, node, inbox string, limits transport.Limits, createdAt uint64) *nip01.Event {
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

// Limits is the transport limits the hub announces.
func (h *Hub) Limits() transport.Limits { return h.limits }

// Mint returns a bill of amountMillis this hub "minted": a fresh wallet key, a
// mint signature by the hub's key, and the hub's relay. cashSecret, when
// non-empty, makes it a cash-mode bill handed out as token#cashSecret.
func (h *Hub) Mint(amountMillis uint64, cashSecret string) (bill, walletPubkey string) {
	h.t.Helper()
	walletPubkey = RandomHex32(h.t)
	payload := nipcash.MintPayload("lokicash", walletPubkey, amountMillis)
	first := sha256.Sum256([]byte(nipcash.LNSignedMessagePrefix + payload))
	second := sha256.Sum256(first[:])
	tok, err := nipcash.Encode(nipcash.Token{
		HRP:                  "lokicash",
		WalletPubkey:         walletPubkey,
		Secret:               RandomHex32(h.t),
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

// ItemsFor returns every item this hub received for method.
func (h *Hub) ItemsFor(method string) []transport.Item {
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

// RequestsTo returns how many kind-23190 requests were p-tagged to inbox.
func (h *Hub) RequestsTo(inbox string) int {
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

// StatusReply answers a cash_status item with one unclaimed, fee-free slice.
func StatusReply(item transport.Item, identityType, identityValue string, amountMillis uint64) *transport.Result {
	return QuoteReply(item, identityType, identityValue, amountMillis, 0)
}

// QuoteReply is StatusReply with a redeem fee.
func QuoteReply(item transport.Item, identityType, identityValue string, amountMillis, feeMillis uint64) *transport.Result {
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

// ErrorReply answers item with error code and message.
func ErrorReply(item transport.Item, code, message string) *transport.Result {
	return &transport.Result{ID: item.ID, ResultType: item.Method, Error: &transport.ResultError{Code: code, Message: message}}
}

// RandomHex32 is 32 random bytes, hex.
func RandomHex32(t testing.TB) string {
	t.Helper()
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}

func (h *Hub) broadcast(ev *nip01.Event) {
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

// takeDrop reports whether this envelope's reply is to be dropped, counting
// it against the matching DropReplies budget.
func (h *Hub) takeDrop(items []transport.Item) bool {
	for _, method := range append([]string{""}, methodsOf(items)...) {
		n, ok := h.dropReplies[method]
		if !ok || n == 0 {
			continue
		}
		if n > 0 {
			h.dropReplies[method] = n - 1
		}
		return true
	}
	return false
}

func methodsOf(items []transport.Item) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Method
	}
	return out
}

// answer is the hub's result for one item, after the hostility knobs.
func (h *Hub) answer(item transport.Item, code string, randomMessages bool) *transport.Result {
	var r *transport.Result
	if code != "" {
		r = ErrorReply(item, code, "fake hub: "+code)
	} else {
		r = h.decide(item)
	}
	if r != nil && r.Error != nil && randomMessages {
		e := *r.Error
		e.Message = "msg-" + RandomHex32(h.t)[:16]
		r.Error = &e
	}
	return r
}

func (h *Hub) handleRequest(ev *nip01.Event) {
	h.mu.Lock()
	if h.dropRequests > 0 {
		h.dropRequests--
		h.mu.Unlock()
		return
	}
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
	drop := h.takeDrop(env.Items)
	lieTotal, lieTotalFor := h.lieTotal, h.lieTotalFor
	randomMessages := h.randomMessages
	codes := make([]string, len(env.Items))
	for i, item := range env.Items {
		codes[i] = h.answerCode[item.Method]
	}
	h.mu.Unlock()
	if drop {
		return
	}

	resp := transport.ResponseEnvelope{Version: transport.EnvelopeVersion, ReqNonce: env.Nonce, Seq: 1, Total: 1}
	for i, item := range env.Items {
		if r := h.answer(item, codes[i], randomMessages); r != nil {
			resp.Results = append(resp.Results, *r)
		}
	}
	if lieTotal > 0 {
		for _, item := range env.Items {
			if item.Method == lieTotalFor {
				resp.Total = lieTotal
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
func (h *Hub) serve() string {
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
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
					h.mu.Lock()
					custom := h.announcements
					h.mu.Unlock()
					anns := []*nip01.Event{h.Announcement(h.inboxXOnly, 0)}
					if custom != nil {
						anns = custom()
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
