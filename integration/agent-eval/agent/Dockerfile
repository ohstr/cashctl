# The agent-under-test's machine. Deliberately minimal and deliberately
# does NOT contain cashctl, Go, or a copy of the cashctl source tree (in
# EVAL_CASHCTL=local mode bin/run.sh copies one built binary in, behind the
# logging shim, which needs python3) — the
# whole point of this harness is that the agent's only knowledge of cashctl
# comes from what it fetches at runtime (docs/PROMPT.md -> AGENTS.md ->
# skills), the same as a real stranger picking it up for the first time.
#
# Unlike ncli's own agent-eval rig, there is no local "relay" service to
# share a network namespace with here: cashctl is a client of an *external*
# Hub (a real, already-running lokihub instance named in
# integration/config.local.yaml), not a server of its own — so this
# container only needs ordinary outbound internet access, nothing more.
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
      bash curl ca-certificates jq git unzip gnupg xz-utils procps python3 \
    && rm -rf /var/lib/apt/lists/*

# Non-root on purpose: Claude Code refuses --dangerously-skip-permissions /
# permission-mode bypassPermissions as root, and running the agent-under-
# test as root inside a throwaway container is the wrong default anyway.
RUN useradd -m -u 10001 -s /bin/bash evaluser
USER evaluser
WORKDIR /home/evaluser

RUN curl -fsSL https://claude.ai/install.sh | bash
ENV PATH="/home/evaluser/.local/bin:${PATH}"

COPY --chown=evaluser:evaluser seed-and-wait.sh /home/evaluser/seed-and-wait.sh
RUN chmod +x /home/evaluser/seed-and-wait.sh && mkdir -p /home/evaluser/work

WORKDIR /home/evaluser/work
ENTRYPOINT ["/home/evaluser/seed-and-wait.sh"]
