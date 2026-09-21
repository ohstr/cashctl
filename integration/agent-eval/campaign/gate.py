#!/usr/bin/env python3
"""Coverage gate for the campaign: compares what the agents actually ran
(the shim's JSONL log) against the full CLI surface (surface.json from
surface-dump) and exits 1 if anything required is missing.

usage: gate.py --surface surface.json --log a.jsonl [--log b.jsonl ...]
               [--waivers waivers.json] [--out gaps.json]

Per runnable command (a command and its shortcut/canonical twin form one
group, coverage is the union of both spellings, but each spelling must have
run at least once):
  ran_each_spelling  exit_ok  exit_fail  mode_json  mode_text
  flag:<name> for every local flag
  prompt:<y|n|enter|eof|yes> for commands that prompt
  connection_flag for wallet commands that honor -c
Per group command (cash, wallet, connect, circle, completion): ran_bare.
Global: flags json/yes/connection/config-dir, --json=true form, NO_COLOR,
unknown-command failure, bare-root run. NCLI_VAULT_PASSWORD is a warning.

A waiver is {"group": "<path>", "req": "<requirement>", "reason": "..."};
waived items are listed in the report, never silently dropped.
"""
import argparse
import json
import sys
from collections import defaultdict

EXTRA_TWINS = [("connect use", "wallet use")]

# Any spelling in the group appearing here means it has interactive prompts.
PROMPT_PATHS = {
    "init", "receive", "decode", "redeem", "transfer", "consolidate", "join", "connect add",
}
PROMPT_ANSWERS = ["y", "n", "enter", "eof"]

# Commands that honor -c/--connection (they dial a stored/raw wallet).
CONNECTION_PATHS = {
    "wallet get-info", "wallet budget", "wallet list-tx", "wallet sign-message",
    "wallet invoice", "wallet pay",
}
GLOBALS = ["json", "yes", "connection", "config-dir"]


def load_log(paths):
    recs = []
    for p in paths:
        with open(p) as f:
            for line in f:
                line = line.strip()
                if line:
                    recs.append(json.loads(line))
    return recs


def build_groups(surface):
    cmds = {c["path"]: c for c in surface["commands"]}
    parent = {p: p for p in cmds}

    def find(x):
        while parent[x] != x:
            parent[x] = parent[parent[x]]
            x = parent[x]
        return x

    def union(a, b):
        if a in parent and b in parent:
            parent[find(a)] = find(b)

    for p, c in cmds.items():
        if c.get("twin"):
            union(p, c["twin"])
    for a, b in EXTRA_TWINS:
        union(a, b)
    groups = defaultdict(list)
    for p in cmds:
        groups[find(p)].append(p)
    return cmds, [sorted(v) for v in groups.values()]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--surface", required=True)
    ap.add_argument("--log", action="append", required=True)
    ap.add_argument("--waivers")
    ap.add_argument("--out")
    args = ap.parse_args()

    surface = json.load(open(args.surface))
    recs = load_log(args.log)
    waivers = {}
    if args.waivers:
        for w in json.load(open(args.waivers)):
            waivers[(w["group"], w["req"])] = w["reason"]

    cmds, groups = build_groups(surface)
    by_path = defaultdict(list)
    for r in recs:
        by_path[r["path"]].append(r)

    gaps, waived, warns = [], [], []

    def need(group_id, req, ok, severity="FAIL"):
        if ok:
            return
        w = waivers.get((group_id, req))
        if w is not None:
            waived.append({"group": group_id, "req": req, "reason": w})
        elif severity == "WARN":
            warns.append({"group": group_id, "req": req})
        else:
            gaps.append({"group": group_id, "req": req})

    for paths in sorted(groups, key=lambda g: g[0]):
        gid = " = ".join(paths)
        runnable = [p for p in paths if cmds[p]["runnable"]]
        if not runnable:  # a bare group command (cash, wallet, ...)
            p = paths[0]
            need(gid, "ran_bare", any(not r["flags"] or r["flags"] == ["help"] for r in by_path[p]) and bool(by_path[p]))
            continue
        rs = [r for p in paths for r in by_path[p]]
        for p in paths:
            need(gid, f"ran:{p}", bool(by_path[p]))
        need(gid, "exit_ok", any(r["exit"] == 0 for r in rs))
        need(gid, "exit_fail", any(r["exit"] != 0 for r in rs))
        need(gid, "mode_json", any(r["mode"] == "json" for r in rs))
        need(gid, "mode_text", any(r["mode"] == "text" for r in rs))
        seen_flags = {f for r in rs for f in r["flags"]}
        for name in sorted({f["name"] for p in paths for f in cmds[p]["flags"]}):
            need(gid, f"flag:{name}", name in seen_flags)
        if any(p in PROMPT_PATHS for p in paths):
            answers = {r["stdin"] for r in rs}
            for a in PROMPT_ANSWERS:
                need(gid, f"prompt:{a}", a in answers)
            need(gid, "prompt:yes_flag", any(r["yes"] for r in rs))
        if any(p in CONNECTION_PATHS for p in paths):
            need(gid, "connection_flag", "connection" in seen_flags)

    allr = recs
    seen_global = {f for r in allr for f in r["flags"]}
    for g in GLOBALS:
        need("<global>", f"flag:{g}", g in seen_global)
    need("<global>", "json_eq_form(--json=true)", any("json" in r["eq_forms"] for r in allr))
    need("<global>", "no_color_env", any(r["no_color"] for r in allr))
    need("<global>", "unknown_command_fails", any(r["path"] == "" and r["exit"] != 0 for r in allr))
    need("<global>", "bare_root_runs", any(r["path"] == "" and r["exit"] == 0 for r in allr))
    need("<global>", "ncli_vault_password_env", any(r["ncli_pw"] for r in allr), severity="WARN")

    total_req = len(gaps) + len(waived) + len(warns)
    print(f"log records: {len(recs)}  agents: {sorted({r['agent'] for r in recs})}")
    print(f"command groups: {len(groups)}  gaps: {len(gaps)}  waived: {len(waived)}  warnings: {len(warns)}")
    by_group = defaultdict(list)
    for g in gaps:
        by_group[g["group"]].append(g["req"])
    for gid in sorted(by_group):
        print(f"  MISSING {gid}: {', '.join(by_group[gid])}")
    for w in waived:
        print(f"  WAIVED  {w['group']}: {w['req']} — {w['reason']}")
    for w in warns:
        print(f"  WARN    {w['group']}: {w['req']}")
    if args.out:
        json.dump({"gaps": gaps, "waived": waived, "warnings": warns, "records": len(recs)}, open(args.out, "w"), indent=1)
    print("GATE: " + ("PASS" if not gaps else "FAIL"))
    sys.exit(0 if not gaps else 1)


if __name__ == "__main__":
    main()
