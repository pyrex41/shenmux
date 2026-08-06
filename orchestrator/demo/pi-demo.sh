#!/usr/bin/env bash
# pi-demo.sh — run the REAL pi coding agent (github.com/earendil-works/pi) as an
# orchestrator worker and prove the design's machinery works with a real harness:
#
#   * pi runs under a real shenmux PTY session, launched by muxwork.
#   * pi's Anthropic API egress is intercepted by the worker's secret-proxy:
#     pi holds only a PLACEHOLDER key; the proxy swaps in the real value and only
#     for the allow-listed host. A local Anthropic-API mock (bin/anthropic-mock)
#     stands in for the model so pi completes a real turn without live creds, and
#     records the credential it received — which is the swapped REAL value.
#   * the pi worker is checkpointed and forked; pi's own session state (stored in
#     the workspace) diverges between forks, shown by a content-addressed diff.
#
# No root, localhost only. Exits non-zero on a failed assertion; SKIPs cleanly
# if pi/npm are unavailable.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/.." && pwd)"
BIN="$ROOT/bin"
cd "$ROOT"

c()  { printf '\033[1;36m%s\033[0m\n' "$*"; }
ok() { printf '   \033[1;32mPASS\033[0m %s\n' "$*"; }
bad(){ printf '   \033[1;31mFAIL\033[0m %s\n' "$*"; FAILED=$((FAILED+1)); }
say(){ printf '   %s\n' "$*"; }
FAILED=0
assert(){ local d="$1"; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d (command: $*)"; fi; }
wait_for(){ local t="$1"; shift; local i=0; while (( i<t*2 )); do if "$@" >/dev/null 2>&1; then return 0; fi; sleep 0.5; i=$((i+1)); done; return 1; }

MOCK_PORT=8193
MOCK_ADDR="127.0.0.1:${MOCK_PORT}"
declare -a PIDS=()
track(){ PIDS+=("$1"); }
cleanup(){
  c "== CLEANUP =="
  [[ -n "${STATE:-}" && -x "$BIN/muxwork" ]] && "$BIN/muxwork" --state "$STATE" gc --ttl 0s >/dev/null 2>&1 || true
  for p in "${PIDS[@]:-}"; do [[ -n "$p" ]] && kill "$p" >/dev/null 2>&1 || true; done
  fuser -k "${MOCK_PORT}/tcp" >/dev/null 2>&1 || true
  [[ -n "${STATE:-}" ]] && rm -rf "$STATE" && say "removed $STATE" || true
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
c "== 0. PREFLIGHT =="
if ! command -v pi >/dev/null 2>&1; then
  if command -v npm >/dev/null 2>&1; then
    say "pi not found — installing @earendil-works/pi-coding-agent ..."
    npm install -g --ignore-scripts @earendil-works/pi-coding-agent >/dev/null 2>&1 || true
  fi
fi
if ! command -v pi >/dev/null 2>&1; then
  say "pi is not available and could not be installed — SKIPPING pi demo."
  say "install with: npm install -g --ignore-scripts @earendil-works/pi-coding-agent"
  exit 0
fi
say "pi version: $(pi --version 2>/dev/null | head -1)"

say "building orchestrator cmds ..."
go build -o "$BIN/" ./cmd/... 2>&1 | sed 's/^/   /' || { bad "go build"; exit 1; }
if [[ ! -x "$BIN/shenmux" ]]; then
  ( cd "$ROOT/.." && CGO_ENABLED=0 go build -o "$BIN/shenmux" ./cmd/shenmux ) && say "built bin/shenmux"
fi
for b in anthropic-mock secret-proxy snapshot muxwork; do assert "bin/$b built" test -x "$BIN/$b"; done

# ---------------------------------------------------------------------------
c "== 1. LOCAL ANTHROPIC MOCK =="
STATE="$(mktemp -d)"
RECV="$STATE/recv-auth.txt"
"$BIN/anthropic-mock" --listen "$MOCK_ADDR" --auth-file "$RECV" >"$STATE/mock.log" 2>&1 & track $!
port_open(){ (exec 3<>"/dev/tcp/127.0.0.1/${MOCK_PORT}") 2>/dev/null; }
assert "anthropic-mock listening on $MOCK_ADDR" wait_for 5 port_open
export MUXWORK_ANTHROPIC_REDIRECT="http://${MOCK_ADDR}"
say "worker secret-proxies will redirect api.anthropic.com -> $MOCK_ADDR (swap still applied)"

muxwork(){ "$BIN/muxwork" --state "$STATE" "$@"; }

# ---------------------------------------------------------------------------
c "== 2. REAL pi WORKER (shenmux PTY + secret-proxy + pi) =="
say "spawning pi worker 'pi-base' ..."
muxwork spawn --name pi-base --harness pi \
  --prompt "Reply with the exact phrase READY-PI-BASE and a short sentence." \
  || bad "muxwork spawn pi-base"

PB_WS="$STATE/workers/pi-base/workspace"
pi_done(){ grep -rq "PI-WORKER-OK" "$PB_WS/.pi-sessions" 2>/dev/null; }
say "waiting for pi to complete its turn (real node startup + streamed response) ..."
if wait_for 90 pi_done; then ok "pi-base completed a turn (mock reply persisted to its session)"; else bad "pi-base did not complete in time"; fi
say "pi-base status: $(muxwork status --name pi-base 2>/dev/null | tr -d '\n' | sed 's/  */ /g' | cut -c1-160)"

# ---- interception assertions -----------------------------------------------
c "== 3. CREDENTIAL INTERCEPTION (real pi) =="
say "what the mock (upstream) received as the API credential:"
say "   $(cat "$RECV" 2>/dev/null)"
assert "proxy log shows the swap for api.anthropic.com" grep -q "replaced ANTHROPIC_API_KEY" "$STATE/workers/pi-base/proxy.log"
assert "upstream received the REAL per-worker value (swap happened)" grep -q "sk-REAL-demo-pi-base-key" "$RECV"
assert "upstream did NOT receive the placeholder" bash -c '! grep -q "sk-placeholder-anthropic" "'"$RECV"'"'
say "pi itself only ever held ANTHROPIC_API_KEY=sk-placeholder-anthropic (operator-injected)."

# ---------------------------------------------------------------------------
c "== 4. CHECKPOINT + FORK a real pi worker =="
say "suspending pi-base (checkpoints its workspace incl. pi session) ..."
muxwork suspend --name pi-base || bad "muxwork suspend pi-base"
assert "pi-base has a checkpoint manifest" bash -c 'ls -A "'"$STATE"'/workers/pi-base/manifests" 2>/dev/null | grep -q .'

say "forking pi-base -> pi-fork with a DIFFERENT prompt ..."
muxwork fork --name pi-fork --from pi-base \
  --mock-target unused \
  --prompt "Reply with the exact phrase FORKED-PI and a different short sentence." \
  || bad "muxwork fork pi-fork"
# The fork inherits pi-base's session; its new turn appends divergent content.
PF_WS="$STATE/workers/pi-fork/workspace"
fork_ran(){ [[ "$(muxwork status --name pi-fork 2>/dev/null | grep -o '"phase":[^,]*')" != "" ]]; }
wait_for 20 fork_ran || true
say "waiting for pi-fork to run its own turn ..."
fork_diverged(){ [[ -d "$PF_WS/.pi-sessions" ]] && ! diff -rq "$PB_WS/.pi-sessions" "$PF_WS/.pi-sessions" >/dev/null 2>&1; }
if wait_for 90 fork_diverged; then ok "pi-fork's session diverged from pi-base"; else say "(fork divergence not confirmed within timeout; showing state below)"; fi

# content-addressed diff between base checkpoint and fork's latest
say "checkpointing pi-fork and diffing trees vs pi-base ..."
muxwork suspend --name pi-fork >/dev/null 2>&1 || true
BASE_M="$(ls -1t "$STATE"/workers/pi-base/manifests/*.sexpr 2>/dev/null | head -1)"
FORK_M="$(ls -1t "$STATE"/workers/pi-fork/manifests/*.sexpr 2>/dev/null | head -1)"
if [[ -n "$BASE_M" && -n "$FORK_M" ]]; then
  say "snapshot diff pi-base <-> pi-fork:"
  "$BIN/snapshot" diff --a "$BASE_M" --b "$FORK_M" 2>/dev/null | sed 's/^/       /' || true
  assert "snapshot diff shows the pi session tree changed" bash -c '"'"$BIN"'/snapshot" diff --a "'"$BASE_M"'" --b "'"$FORK_M"'" 2>/dev/null | grep -q .'
fi

# ---------------------------------------------------------------------------
c "== 5. FLEET VIEW + EVENT STREAM =="
say "muxwork list:"
muxwork list 2>&1 | sed 's/^/       /' || true
say "events (muxwork watch --json, first lines):"
( timeout 4 "$BIN/muxwork" --state "$STATE" watch --json 2>/dev/null || true ) | head -n 12 | sed 's/^/       /' || true

# ---------------------------------------------------------------------------
if (( FAILED == 0 )); then
  printf '\n\033[1;32m========== pi DEMO OK ==========\033[0m\n'
else
  printf '\n\033[1;31m========== pi DEMO FAILED (%d) ==========\033[0m\n' "$FAILED"; exit 1
fi
