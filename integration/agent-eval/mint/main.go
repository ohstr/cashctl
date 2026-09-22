// Command mint is the campaign harness's fixture service: it holds the
// lokihub admin token so the test/review agents never do, and hands them
// only what they ask for (a cash token, a wallet connection, a circle hub
// connection, an invoice), within per-agent quotas. Everything it creates is
// recorded in a state file so `mint sweep` can reclaim it. Not part of
// cashctl itself; run with `go run` or a built copy, never shipped.
//
//	mint token       --agent A [--hub L] (--for <hex|npub> | --cash) [--amount MLOKI]
//	                 [--signed] [--expires-in SECS] [--fee-ppm N] [--min-transfer MLOKI]
//	mint invoice     --agent A [--hub L] --amount LOKI
//	mint nwc-wallet  --agent A [--kind plain|pay|signer]
//	mint circle-hub  --agent A [--allow <hex|npub>]... [--max-exp-secs N] [--fees-ppm N] [--expired]
//	mint fake        --agent A --kind cash-hub|circle-hub|nwc|nconnection|token|token-cash|token-pubkey|token-signed
//	mint status      --agent A
//	mint sweep       [--agent A]
//
// Env: MINT_CONFIG (default integration/config.local.yaml), MINT_STATE
// (default ./mint-state), MINT_RUN (name prefix tag, default "run").
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
	btcec "github.com/flokiorg/go-flokicoin/crypto"
	"github.com/ohstr/nmilat/nip19"
	"github.com/ohstr/nmilat/nip47"
	"github.com/ohstr/nmilat/nipcash"
	nipcashclient "github.com/ohstr/nmilat/nipcash/client"
	"github.com/ohstr/nmilat/nipcw"
	relayclient "github.com/ohstr/nmilat/relay/client"
	"gopkg.in/yaml.v3"
)

const namePrefix = "cashctl-campaign"

// Quotas: a mistaken or hostile agent can move at most this much through
// the mint tool, whatever it asks for.
const (
	maxTokenMloki      = 100_000 // per token
	maxAgentMloki      = 800_000 // total minted per agent
	maxHubsPerAgent    = 6
	maxWalletsPerAgent = 4
	maxCirclesPerAgent = 3
	hubFundLoki        = 300
	circleFundLoki     = 100
	hubPerWalletMax    = 1_000_000
)

type appRec struct {
	AppID   uint   `json:"app_id"`
	Pairing string `json:"pairing,omitempty"`
	Label   string `json:"label,omitempty"`
	FeePpm  int    `json:"fee_ppm,omitempty"`
	MinXfer int64  `json:"min_transfer,omitempty"`
}

type agentState struct {
	MintedMloki uint64            `json:"minted_mloki"`
	Hubs        map[string]appRec `json:"hubs"`
	Wallets     []appRec          `json:"wallets"`
	Circles     []appRec          `json:"circles"`
}

type state struct {
	Agents map[string]*agentState `json:"agents"`
}

type admin struct {
	base, token string
}

func main() {
	if len(os.Args) < 2 {
		fatalf("usage: mint token|invoice|nwc-wallet|circle-hub|fake|status|sweep [flags]")
	}
	cmd, args := os.Args[1], os.Args[2:]

	cfg := loadConfig()
	a := &admin{base: strings.TrimRight(cfg.AdminAPI.BaseURL, "/"), token: cfg.AdminAPI.Token}

	stateDir := envOr("MINT_STATE", "mint-state")
	must(os.MkdirAll(stateDir, 0o700))
	unlock := lock(filepath.Join(stateDir, "lock"))
	defer unlock()
	st := loadState(filepath.Join(stateDir, "state.json"))
	defer saveState(filepath.Join(stateDir, "state.json"), st)

	switch cmd {
	case "token":
		cmdToken(a, st, args)
	case "invoice":
		cmdInvoice(st, args)
	case "nwc-wallet":
		cmdWallet(a, st, args)
	case "circle-hub":
		cmdCircle(a, st, args)
	case "fake":
		cmdFake(args)
	case "status":
		cmdStatus(st, args)
	case "sweep":
		cmdSweep(a, st, args)
	default:
		fatalf("unknown subcommand %q", cmd)
	}
}

// ---- token ----

func cmdToken(a *admin, st *state, args []string) {
	fs := flag.NewFlagSet("token", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name (required)")
	label := fs.String("hub", "default", "hub label: same label = same hub = same minter")
	forPub := fs.String("for", "", "recipient pubkey (hex or npub) for a pubkey-mode token")
	cashMode := fs.Bool("cash", false, "mint a cash-mode token instead")
	amount := fs.Uint64("amount", 50_000, "amount in mloki")
	signed := fs.Bool("signed", false, "request a mint signature (best effort server-side)")
	expires := fs.Int("expires-in", 0, "per-mint expiry seconds (0 = hub ceiling)")
	feePpm := fs.Int("fee-ppm", 0, "redeem fee ppm (applies when the hub label is first created)")
	minXfer := fs.Int64("min-transfer", 0, "min transfer floor mloki (applies when the hub label is first created)")
	must(fs.Parse(args))
	ag := agentFor(st, *agent)

	if (*forPub == "") == !*cashMode {
		fatalf("token: pass exactly one of --for <pubkey> or --cash")
	}
	if *amount == 0 || *amount > maxTokenMloki {
		fatalf("token: amount must be 1..%d mloki (quota)", maxTokenMloki)
	}
	if ag.MintedMloki+*amount > maxAgentMloki {
		fatalf("token: agent %q would exceed its %d mloki total mint quota (used %d)", *agent, maxAgentMloki, ag.MintedMloki)
	}
	hub, ok := ag.Hubs[*label]
	if ok && (hub.FeePpm != *feePpm || hub.MinXfer != *minXfer) && (*feePpm != 0 || *minXfer != 0) {
		fatalf("token: hub %q already exists with fee-ppm=%d min-transfer=%d; use a new --hub label for different policy", *label, hub.FeePpm, hub.MinXfer)
	}
	if !ok {
		if len(ag.Hubs) >= maxHubsPerAgent {
			fatalf("token: agent %q already has %d hubs (quota)", *agent, maxHubsPerAgent)
		}
		resp, err := a.createApp(map[string]any{
			"name":                  fmt.Sprintf("%s %s %s cash_hub %s", namePrefix, envOr("MINT_RUN", "run"), *agent, *label),
			"kind":                  "cash_hub",
			"scopes":                []string{"cash_hub", "pay_invoice", "make_invoice", "get_balance"},
			"cashPerWalletMaxMloki": hubPerWalletMax,
			"cashMaxExpSecs":        3600,
			"cashRedeemFeePpm":      *feePpm,
			"cashMinTransferMloki":  *minXfer,
		})
		must(err)
		hub = appRec{AppID: resp.ID, Pairing: resp.PairingUri, Label: *label, FeePpm: *feePpm, MinXfer: *minXfer}
		ag.Hubs[*label] = hub // recorded before funding so a failed fund is still swept
		saveState(filepath.Join(envOr("MINT_STATE", "mint-state"), "state.json"), st)
		must(a.doBody(http.MethodPost, "/api/transfers", map[string]any{"toAppId": resp.ID, "amountLoki": hubFundLoki}, nil))
	}

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	client, err := nipcashclient.Connect(ctx, hub.Pairing)
	must(err)
	defer client.Close()

	target := nipcash.Anyone()
	if !*cashMode {
		target = nipcash.Pubkey(toHex(*forPub))
	}
	res, err := client.MintCash(ctx, nipcash.MintCashParams{
		Recipients:    []nipcash.Allocation{nipcash.Send(target, *amount)},
		Expiry:        time.Duration(*expires) * time.Second,
		MintSignature: *signed,
	})
	must(err)
	ag.MintedMloki += *amount

	out := map[string]any{"cash_token": res.CashToken, "hub": *label, "hub_app_id": hub.AppID, "amount_millis": *amount}
	if *cashMode {
		if len(res.Recipients) != 1 || res.Recipients[0].CashSecret == "" {
			fatalf("token: hub returned no cash secret: %+v", res.Recipients)
		}
		out["cash_secret"] = res.Recipients[0].CashSecret
		out["gift"] = res.CashToken + "#" + res.Recipients[0].CashSecret
	}
	printJSON(out)
}

// ---- invoice ----

func cmdInvoice(st *state, args []string) {
	fs := flag.NewFlagSet("invoice", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name (required)")
	label := fs.String("hub", "default", "hub label whose hub makes the invoice (redeem pays it from that same hub)")
	amountLoki := fs.Int64("amount", 10, "invoice amount in loki")
	must(fs.Parse(args))
	ag := agentFor(st, *agent)
	hub, ok := ag.Hubs[*label]
	if !ok {
		fatalf("invoice: agent %q has no hub %q yet (mint a token from it first)", *agent, *label)
	}
	if *amountLoki <= 0 || *amountLoki > 100 {
		fatalf("invoice: amount must be 1..100 loki (quota)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pairing, err := nip47.ParsePairingURI(hub.Pairing)
	must(err)
	c, err := relayclient.NewNWCClient(ctx, pairing, nip47.EncryptionNIP44V2)
	must(err)
	defer c.Close()
	inv, err := c.MakeInvoice(ctx, nip47.MakeInvoiceParams{Amount: *amountLoki * 1000})
	must(err)
	printJSON(map[string]any{"invoice": inv.Invoice, "hub": *label, "amount_loki": *amountLoki})
}

// ---- nwc-wallet ----

func cmdWallet(a *admin, st *state, args []string) {
	fs := flag.NewFlagSet("nwc-wallet", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name (required)")
	kind := fs.String("kind", "plain", "plain (get_info,get_balance) | pay (isolated, funded, can pay/make invoices) | signer (sign_message only)")
	must(fs.Parse(args))
	ag := agentFor(st, *agent)
	if len(ag.Wallets) >= maxWalletsPerAgent {
		fatalf("nwc-wallet: agent %q already has %d wallets (quota)", *agent, maxWalletsPerAgent)
	}
	req := map[string]any{"name": fmt.Sprintf("%s %s %s %s wallet", namePrefix, envOr("MINT_RUN", "run"), *agent, *kind)}
	fund := false
	switch *kind {
	case "plain":
		req["scopes"] = []string{"get_info", "get_balance"}
	case "signer":
		req["scopes"] = []string{"sign_message"}
	case "pay":
		req["scopes"] = []string{"get_info", "get_balance", "make_invoice", "pay_invoice"}
		req["kind"] = "isolated"
		fund = true
	default:
		fatalf("nwc-wallet: unknown kind %q", *kind)
	}
	resp, err := a.createApp(req)
	must(err)
	ag.Wallets = append(ag.Wallets, appRec{AppID: resp.ID, Pairing: resp.PairingUri, Label: *kind})
	saveState(filepath.Join(envOr("MINT_STATE", "mint-state"), "state.json"), st)
	if fund {
		must(a.doBody(http.MethodPost, "/api/transfers", map[string]any{"toAppId": resp.ID, "amountLoki": 100}, nil))
	}
	printJSON(map[string]any{"uri": resp.PairingUri, "app_id": resp.ID, "kind": *kind})
}

// ---- circle-hub ----

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func cmdCircle(a *admin, st *state, args []string) {
	fs := flag.NewFlagSet("circle-hub", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name (required)")
	var allow multi
	fs.Var(&allow, "allow", "pubkey (hex or npub) to allowlist; repeatable")
	maxExp := fs.Int("max-exp-secs", 86400, "circle wallet max expiry seconds")
	fees := fs.Int("fees-ppm", 0, "circle forwarding fee ppm")
	expired := fs.Bool("expired", false, "create the hub already expired")
	must(fs.Parse(args))
	ag := agentFor(st, *agent)
	if len(ag.Circles) >= maxCirclesPerAgent {
		fatalf("circle-hub: agent %q already has %d circle hubs (quota)", *agent, maxCirclesPerAgent)
	}
	tag := fmt.Sprintf("%s %s %s circle_hub", namePrefix, envOr("MINT_RUN", "run"), *agent)
	req := map[string]any{
		"name": tag, "kind": "circle_hub", "scopes": []string{"circle_wallet"},
		"circleIdentityName": tag + " identity", "circlePolicy": "allowlist",
		"circleMaxExpSecs": *maxExp, "circlePerWalletMaxMloki": 1_000_000, "circleFeesPpm": *fees,
	}
	if *expired {
		req["expiresAt"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	}
	resp, err := a.createApp(req)
	must(err)
	ag.Circles = append(ag.Circles, appRec{AppID: resp.ID, Pairing: resp.PairingUri})
	saveState(filepath.Join(envOr("MINT_STATE", "mint-state"), "state.json"), st)
	must(a.doBody(http.MethodPost, "/api/transfers", map[string]any{"toAppId": resp.ID, "amountLoki": circleFundLoki}, nil))
	if len(allow) > 0 {
		var cur struct {
			Pubkeys []string `json:"pubkeys"`
		}
		must(a.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d/circle/allowlist", resp.ID), nil, &cur))
		for _, p := range allow {
			cur.Pubkeys = append(cur.Pubkeys, toHex(p))
		}
		must(a.doBody(http.MethodPut, fmt.Sprintf("/api/apps/%d/circle/allowlist", resp.ID), map[string]any{"pubkeys": cur.Pubkeys}, nil))
	}
	tok := ""
	if resp.CircleHubToken != nil {
		tok = *resp.CircleHubToken
	}
	printJSON(map[string]any{"circle_hub": tok, "pairing_uri": resp.PairingUri, "app_id": resp.ID})
}

// ---- fake ----

// cmdFake prints a syntactically valid but never-funded value — for
// mis-paste and parser scenarios that must not touch (or cost) anything.
// Offline: no admin call, no quota, nothing recorded.
func cmdFake(args []string) {
	fs := flag.NewFlagSet("fake", flag.ExitOnError)
	_ = fs.String("agent", "", "agent name (accepted for wrapper symmetry)")
	kind := fs.String("kind", "", "cash-hub | circle-hub | nwc | nconnection | token | token-cash | token-pubkey | token-signed")
	must(fs.Parse(args))
	rnd := func() string {
		b := make([]byte, 32)
		_, err := rand.Read(b)
		must(err)
		return hex.EncodeToString(b)
	}
	yes, no := true, false
	tok := func(idReq *bool) string {
		v, err := nipcash.Encode(nipcash.Token{HRP: "lokicash", WalletPubkey: rnd(), Secret: rnd(), RelayURLs: []string{"wss://fake.invalid"}, IdentityRequired: idReq})
		must(err)
		return v
	}
	out := map[string]any{"kind": *kind}
	switch *kind {
	case "cash-hub":
		v, err := nipcash.EncodeCashHubConnection(nipcash.CashHubConnection{WalletPubkey: rnd(), Secret: rnd(), RelayURLs: []string{"wss://fake.invalid"}})
		must(err)
		out["value"] = v
	case "circle-hub":
		v, err := nipcw.EncodeCircleHubConnection(nipcw.CircleHubConnection{WalletPubkey: rnd(), Secret: rnd(), RelayURLs: []string{"wss://fake.invalid"}})
		must(err)
		out["value"] = v
	case "nwc":
		out["value"] = "nostr+walletconnect://" + rnd() + "?relay=wss%3A%2F%2Ffake.invalid&secret=" + rnd()
	case "nconnection":
		out["value"] = "nconnection1qqs2u2jjqsyulm0ruq4xpx2xgxp3jyrxwsemlmsm5vudxv92sqz4tjgpzamhxue69uhhyetvv9ujuetcv9khqmr99e3k7mgzqajxjumrdaexg3g904n"
	case "token":
		out["value"] = tok(nil)
	case "token-pubkey":
		out["value"] = tok(&yes)
	case "token-cash":
		out["value"] = tok(&no)
	case "token-signed":
		priv, pub := btcec.PrivKeyFromBytes([]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 21, 22, 23, 24, 25, 26, 27, 28, 29, 30, 31, 32})
		const amount = uint64(1000)
		wp := rnd()
		first := sha256.Sum256([]byte(nipcash.LNSignedMessagePrefix + nipcash.MintPayload("lokicash", wp, amount)))
		second := sha256.Sum256(first[:])
		amt := amount
		v, err := nipcash.Encode(nipcash.Token{HRP: "lokicash", WalletPubkey: wp, Secret: rnd(), RelayURLs: []string{"wss://fake.invalid"},
			MintSignature: ecdsa.SignCompact(priv, second[:], true), AttestedAmountMillis: &amt})
		must(err)
		ser := btcec.ToSerialized(pub)
		out["value"], out["minter_pubkey"] = v, hex.EncodeToString(ser[:])
	default:
		fatalf("fake: unknown --kind %q", *kind)
	}
	printJSON(out)
}

// ---- status / sweep ----

func cmdStatus(st *state, args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	agent := fs.String("agent", "", "agent name (required)")
	must(fs.Parse(args))
	ag := agentFor(st, *agent)
	printJSON(map[string]any{
		"minted_mloki": ag.MintedMloki, "mint_quota_mloki": maxAgentMloki,
		"hubs": len(ag.Hubs), "wallets": len(ag.Wallets), "circle_hubs": len(ag.Circles),
	})
}

func cmdSweep(a *admin, st *state, args []string) {
	fs := flag.NewFlagSet("sweep", flag.ExitOnError)
	agent := fs.String("agent", "", "limit to one agent (default: all, plus any app matching the name prefix)")
	must(fs.Parse(args))

	type target struct {
		id   uint
		kind string // hub | circle | plain
	}
	var targets []target
	seen := map[uint]bool{}
	add := func(id uint, kind string) {
		if !seen[id] {
			seen[id] = true
			targets = append(targets, target{id, kind})
		}
	}
	for name, ag := range st.Agents {
		if *agent != "" && name != *agent {
			continue
		}
		for _, h := range ag.Hubs {
			add(h.AppID, "hub")
		}
		for _, c := range ag.Circles {
			add(c.AppID, "circle")
		}
		for _, w := range ag.Wallets {
			add(w.AppID, "plain")
		}
	}
	if *agent == "" { // prefix scan catches anything a crashed run recorded nowhere
		var apps struct {
			Apps []struct {
				ID   uint   `json:"id"`
				Name string `json:"name"`
				Kind string `json:"kind"`
			} `json:"apps"`
		}
		must(a.doBody(http.MethodGet, "/api/apps?limit=0", nil, &apps))
		for _, ap := range apps.Apps {
			if strings.HasPrefix(ap.Name, namePrefix) {
				k := "plain"
				switch ap.Kind {
				case "cash_hub":
					k = "hub"
				case "circle_hub":
					k = "circle"
				}
				add(ap.ID, k)
			}
		}
	}

	swept, failed := 0, 0
	for _, t := range targets {
		switch t.kind {
		case "hub":
			var claims struct {
				Claims []struct {
					WalletAppID uint `json:"wallet_app_id"`
				} `json:"claims"`
			}
			if err := a.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d/cash-wallets?limit=0", t.id), nil, &claims); err == nil {
				done := map[uint]bool{}
				for _, c := range claims.Claims {
					if !done[c.WalletAppID] {
						done[c.WalletAppID] = true
						_ = a.deleteRetry(fmt.Sprintf("/api/apps/%d/cash-wallets/%d", t.id, c.WalletAppID))
					}
				}
			}
		case "circle":
			var kids struct {
				Children []struct {
					AppID uint `json:"appId"`
				} `json:"children"`
			}
			if err := a.doBody(http.MethodGet, fmt.Sprintf("/api/apps/%d/circle/children?limit=0", t.id), nil, &kids); err == nil {
				for _, c := range kids.Children {
					_ = a.deleteRetry(fmt.Sprintf("/api/apps/%d/circle/children/%d", t.id, c.AppID))
				}
			}
		}
		if err := a.deleteRetry(fmt.Sprintf("/api/apps/%d", t.id)); err != nil {
			failed++
			fmt.Fprintf(os.Stderr, "sweep: app %d (%s): %v\n", t.id, t.kind, err)
			continue
		}
		swept++
	}
	if *agent == "" && failed == 0 {
		st.Agents = map[string]*agentState{}
	}
	printJSON(map[string]any{"swept": swept, "failed": failed})
	if failed > 0 {
		os.Exit(1)
	}
}

// ---- admin API ----

type createResp struct {
	ID             uint    `json:"id"`
	PairingUri     string  `json:"pairingUri"`
	CircleHubToken *string `json:"circleHubToken,omitempty"`
}

func (a *admin) createApp(req map[string]any) (createResp, error) {
	var r createResp
	err := a.doBody(http.MethodPost, "/api/apps", req, &r)
	return r, err
}

// deleteRetry DELETEs path, retrying on the server's transient "still
// settling" guard the same way the integration suite's own cleanup does. A
// 404 counts as success (already gone).
func (a *admin) deleteRetry(path string) error {
	var err error
	for attempt := 1; attempt <= 18; attempt++ {
		err = a.doBody(http.MethodDelete, path, nil, nil)
		if err == nil || strings.Contains(err.Error(), "status 404") {
			return nil
		}
		if !strings.Contains(err.Error(), "still settling") {
			return err
		}
		// a circle wallet can sit "settling" for a while after a payment;
		// back off up to ~10s per try, ~2 minutes in total
		d := time.Duration(attempt) * time.Second
		if d > 10*time.Second {
			d = 10 * time.Second
		}
		time.Sleep(d)
	}
	return err
}

func (a *admin) doBody(method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, a.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("admin API %s %s: status %d: %s", method, path, resp.StatusCode, bytes.TrimSpace(rb))
	}
	if out != nil && len(rb) > 0 {
		return json.Unmarshal(rb, out)
	}
	return nil
}

// ---- plumbing ----

type config struct {
	AdminAPI struct {
		BaseURL string `yaml:"base_url"`
		Token   string `yaml:"token"`
	} `yaml:"admin_api"`
}

func loadConfig() config {
	b, err := os.ReadFile(envOr("MINT_CONFIG", "integration/config.local.yaml"))
	must(err)
	var c config
	must(yaml.Unmarshal(b, &c))
	if c.AdminAPI.BaseURL == "" || c.AdminAPI.Token == "" {
		fatalf("mint: admin_api.base_url/token missing in config")
	}
	return c
}

func agentFor(st *state, name string) *agentState {
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}$`).MatchString(name) {
		fatalf("--agent is required (letters, digits, - and _ only)")
	}
	if st.Agents == nil {
		st.Agents = map[string]*agentState{}
	}
	ag := st.Agents[name]
	if ag == nil {
		ag = &agentState{Hubs: map[string]appRec{}}
		st.Agents[name] = ag
	}
	if ag.Hubs == nil {
		ag.Hubs = map[string]appRec{}
	}
	return ag
}

func toHex(s string) string {
	if strings.HasPrefix(s, "npub1") {
		h, err := nip19.DecodePublicKey(s)
		must(err)
		return h
	}
	if !regexp.MustCompile(`^[0-9a-fA-F]{64}$`).MatchString(s) {
		fatalf("not a hex pubkey or npub: %q", s)
	}
	return strings.ToLower(s)
}

func lock(path string) func() {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	must(err)
	must(syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }
}

func loadState(path string) *state {
	st := &state{Agents: map[string]*agentState{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st
	}
	must(err)
	must(json.Unmarshal(b, st))
	return st
}

func saveState(path string, st *state) {
	b, err := json.MarshalIndent(st, "", " ")
	must(err)
	must(os.WriteFile(path, b, 0o600))
}

func printJSON(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "mint:", err)
		os.Exit(1)
	}
}

func fatalf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "mint: "+f+"\n", a...)
	os.Exit(1)
}
