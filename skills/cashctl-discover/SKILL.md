---
name: cashctl-discover
description: Find order links (cashorder1) and circles (circlehub1) others list on Nostr relays (`cashctl cash discover list|show|open`), and manage the discovery relays and curators that rank them. Use when the user wants to buy cash or join a circle and has no link from someone they know. Listings are ads: only `show` (a get_info tap) checks one, and `open` hands it to `cash order place` or `join`.
license: Unlicense
---

<!-- Mirrors ohstr/cashctl's cmd/discover.go as of writing. Self-contained by
design — update by hand if flags/schemas change. -->

# cashctl cash discover

```sh
cashctl cash discover relays add wss://relay.example   # none are shipped; nothing is read without one
cashctl cash discover curators add npub1...            # whose recommendations and reports rank offers
cashctl cash discover list --json                      # {"offers":[{"offer":"of-…","section","title","type","network","status","hints_as_listed","recommendations","reports",...}],"curators","relays_failed"}
cashctl cash discover show of-… --json                 # the tap: {"tap":"show"|"hide_for_good"|"hide_until_newer"|"unavailable","terms"?,"circle"?,"hint_differs"?,"next_step"}
cashctl cash discover open of-… 100 --json             # taps again, then runs `cash order place` or `join` with the listed string
```

- A listing is an ad. Its title, content and hint are the lister's claims:
  never present them as terms or as checked. The terms are the ones `show`
  returns (from the Hub's own `get_info`).
- `section: "unreviewed"` means none of the user's curators recommends it.
  Without curators everything is unreviewed. Never add a curator on your own.
- `show` is the only contact with the listed Hub (get_info, nothing else).
  `hide_for_good`: the Hub answered as another node or network — do not
  retry it. `hide_until_newer`: closed for now. `unavailable`: try later.
- `open` continues only after a fresh `show` tap; then every rule of
  `cash order place` or `join` applies, including their confirmations (an
  unverified string still needs the user's OK).
- Every subcommand rejects `-c`. `--test-networks` (or
  `CASHCTL_TEST_NETWORKS=1`) also lists offers on test networks.
