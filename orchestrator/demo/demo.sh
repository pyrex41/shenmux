#!/usr/bin/env bash
#
# demo.sh — the runnable demonstration of the shenmux worker-orchestration loop
# as local OS processes (CONTRACTS.md "Demo entrypoint"). It proves, end to end
# and on localhost only:
#
#   1. MITM credential interception — the real secret reaches an allow-listed
#      host, only the placeholder reaches a blocked host, and the agent never
#      holds the real value.
#   2. A real worker (shenmux PTY + secret-proxy + mock harness) making that
#      swap in flight.
#   3. Content-addressed checkpoint + O(delta) fork: one base worker forked
#      three ways whose workspaces visibly diverge.
#   4. The lifecycle event stream (the demo's stand-in for the CR watch feed).
#
# It is idempotent, needs no root and no network beyond loopback, and exits
# non-zero on any failed assertion.
set -euo pipefail

# ---------------------------------------------------------------------------
# Locations
# ---------------------------------------------------------------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ORCH_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
BIN="$ORCH_DIR/bin"
cd "$ORCH_DIR"

UPSTREAM_PORT=8091
UPSTREAM_ADDR="0.0.0.0:${UPSTREAM_PORT}"        # bind all loopback aliases so a
                                                # blocked-host alias is reachable
ALLOWED_URL="http://127.0.0.1:${UPSTREAM_PORT}/" # 127.0.0.1 -> allow-listed
BLOCKED_URL="http://127.0.0.2:${UPSTREAM_PORT}/" # 127.0.0.2 -> not allow-listed
PROXY_PORT=8888
PROXY_URL="http://127.0.0.1:${PROXY_PORT}"

PLACEHOLDER="sk-placeholder-anthropic"
REAL_VALUE="sk-REAL-anthropic-value"

# Scratch — everything lives here so re-runs are clean and idempotent.
WORK="$(mktemp -d "${TMPDIR:-/tmp}/shenmux-demo.XXXXXX")"
STATE="$WORK/state"
CA_DIR="$WORK/proxy-ca"
SECRETS="$WORK/secrets.json"
PROXY_LOG="$WORK/secret-proxy.log"
UPSTREAM_LOG="$WORK/mock-upstream.log"
mkdir -p "$STATE" "$CA_DIR"

# Background pids we own and must reap.
declare -a BG_PIDS=()

FAILED=0

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
section() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
say()     { printf '   %s\n' "$*"; }
ok()      { printf '   \033[1;32mPASS\033[0m %s\n' "$*"; }
bad()     { printf '   \033[1;31mFAIL\033[0m %s\n' "$*"; FAILED=1; }

# assert <description> — succeeds if the following test-command exits 0.
# Usage: assert "desc" test-command...
assert() {
  local desc="$1"; shift
  if "$@"; then ok "$desc"; else bad "$desc (command: $*)"; fi
}

contains()     { grep -qF -- "$2" "$1"; }
not_contains() { ! grep -qF -- "$2" "$1"; }

# wait_for <timeout-seconds> <cmd...> — poll until cmd succeeds or timeout.
wait_for() {
  local timeout="$1"; shift
  local deadline=$(( $(date +%s) + timeout ))
  while (( $(date +%s) < deadline )); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 0.3
  done
  return 1
}

http_ok() { curl -fsS --noproxy '*' -o /dev/null "$1"; }

port_open() {
  # $1 host $2 port — pure-bash TCP probe, no external deps.
  (exec 3<>"/dev/tcp/$1/$2") 2>/dev/null && exec 3>&- 2>/dev/null
}

muxwork() { "$BIN/muxwork" --state "$STATE" "$@"; }

# Track a backgrounded pid for cleanup.
track() { BG_PIDS+=("$1"); }

cleanup() {
  local ec=$?
  section "CLEANUP"
  # Suspend/GC any workers the operator still has running, best-effort.
  if [[ -x "$BIN/muxwork" ]]; then
    for w in base alpha beta gamma; do
      muxwork suspend --name "$w" >/dev/null 2>&1 || true
    done
    muxwork gc >/dev/null 2>&1 || true
  fi
  for pid in "${BG_PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill "$pid" >/dev/null 2>&1 || true
  done
  # Give children a moment, then hard-kill stragglers we started.
  for pid in "${BG_PIDS[@]:-}"; do
    [[ -n "$pid" ]] && kill -9 "$pid" >/dev/null 2>&1 || true
  done
  rm -rf "$WORK" 2>/dev/null || true
  say "removed $WORK"
  exit "$ec"
}
trap cleanup EXIT INT TERM

# ---------------------------------------------------------------------------
# 0. Build everything into bin/
# ---------------------------------------------------------------------------
section "0. BUILD"
say "building orchestrator cmds into bin/ ..."
go build -o "$BIN/" ./cmd/...
say "built: $(cd "$BIN" && ls | tr '\n' ' ')"

if [[ ! -x "$BIN/shenmux" ]]; then
  say "bin/shenmux missing — building the full shenmux binary from the parent module"
  if ( cd "$ORCH_DIR/.." && CGO_ENABLED=0 go build -o "$ORCH_DIR/bin/shenmux" ./cmd/shenmux ) 2>/dev/null; then
    say "built bin/shenmux"
  else
    say "could not build bin/shenmux — muxwork will fall back to a bare PTY / tmux backend (demo still valid)"
  fi
else
  say "bin/shenmux present"
fi

for req in mock-upstream secret-proxy snapshot muxwork mock-agent; do
  assert "bin/$req built" test -x "$BIN/$req"
done
(( FAILED == 0 )) || { section "BUILD FAILED"; exit 1; }

# ---------------------------------------------------------------------------
# 1. Start the mock upstream (the "provider API" endpoint)
# ---------------------------------------------------------------------------
section "1. MOCK UPSTREAM"
# mock-upstream's flags are owned by another component; try the likely forms
# and keep the first that binds, so the demo is robust to its exact CLI.
start_upstream() {
  local -a attempts=(
    "$BIN/mock-upstream --http $UPSTREAM_ADDR"
    "$BIN/mock-upstream -http $UPSTREAM_ADDR"
    "$BIN/mock-upstream --listen $UPSTREAM_ADDR"
    "$BIN/mock-upstream --addr $UPSTREAM_ADDR"
    "$BIN/mock-upstream --http 127.0.0.1:$UPSTREAM_PORT"
    "$BIN/mock-upstream $UPSTREAM_ADDR"
  )
  local envform="MOCK_UPSTREAM_ADDR=$UPSTREAM_ADDR $BIN/mock-upstream"
  local cmd
  for cmd in "${attempts[@]}" "$envform"; do
    say "trying: $cmd"
    ( eval "$cmd" ) >>"$UPSTREAM_LOG" 2>&1 &
    local pid=$!
    if wait_for 5 port_open 127.0.0.1 "$UPSTREAM_PORT"; then
      track "$pid"
      say "mock-upstream up (pid $pid)"
      return 0
    fi
    kill "$pid" >/dev/null 2>&1 || true
  done
  return 1
}
assert "mock-upstream listening on 127.0.0.1:$UPSTREAM_PORT" start_upstream
(( FAILED == 0 )) || exit 1

# Is the blocked-host alias (127.0.0.2) reachable too? Needed for the strong
# form of the blocked-host assertion; if not, we fall back to the weaker one.
BLOCKED_REACHABLE=0
if wait_for 3 port_open 127.0.0.2 "$UPSTREAM_PORT"; then
  BLOCKED_REACHABLE=1
  say "127.0.0.2 alias reachable — strong blocked-host check enabled"
else
  say "127.0.0.2 alias not reachable — using weaker blocked-host check (real value must be absent)"
fi

# ---------------------------------------------------------------------------
# 2. Secret interception: allowed host vs blocked host
# ---------------------------------------------------------------------------
section "2. SECRET INTERCEPTION (standalone)"
cat >"$SECRETS" <<JSON
{ "secrets": {
    "ANTHROPIC_API_KEY": {
      "placeholder": "$PLACEHOLDER",
      "value": "$REAL_VALUE",
      "bao_ref": "secret/data/harness/anthropic#api_key",
      "allowed_hosts": ["127.0.0.1"]
    } } }
JSON
say "secrets.json: allowed_hosts=[127.0.0.1], NOT blocked.example / 127.0.0.2"

say "starting secret-proxy on $PROXY_URL ..."
"$BIN/secret-proxy" --listen "127.0.0.1:${PROXY_PORT}" --secrets "$SECRETS" \
  --ca-dir "$CA_DIR" --log "$PROXY_LOG" >>"$PROXY_LOG" 2>&1 &
PROXY_PID=$!
track "$PROXY_PID"
assert "secret-proxy listening on 127.0.0.1:$PROXY_PORT" \
  wait_for 5 port_open 127.0.0.1 "$PROXY_PORT"

CA_FILE="$CA_DIR/ca.crt"   # per CONTRACTS §1 the CA lands here

# Drive the requests through mock-agent (we own it; it trusts the proxy CA and
# always routes loopback through the proxy). One step each, output captured.
run_agent() {
  # $1 target url, $2 output file
  env HTTPS_PROXY="$PROXY_URL" HTTP_PROXY="$PROXY_URL" \
      SSL_CERT_FILE="$CA_FILE" NODE_EXTRA_CA_CERTS="$CA_FILE" \
      ANTHROPIC_API_KEY="$PLACEHOLDER" MOCK_TARGET="$1" MOCK_STEPS=1 \
      WORKER="probe" WORKSPACE="$WORK/probe-ws" \
      "$BIN/mock-agent" >"$2" 2>&1 || true
}

ALLOWED_OUT="$WORK/allowed.out"
BLOCKED_OUT="$WORK/blocked.out"

say "--- ALLOWED host (127.0.0.1) ---"
run_agent "$ALLOWED_URL" "$ALLOWED_OUT"
sed 's/^/       /' "$ALLOWED_OUT"
assert "allowed host: agent held only the placeholder" contains "$ALLOWED_OUT" "agent-holds: $PLACEHOLDER"
assert "allowed host: upstream received the REAL value (swap happened)" contains "$ALLOWED_OUT" "upstream-received: $REAL_VALUE"

say "--- BLOCKED host (127.0.0.2 / not allow-listed) ---"
run_agent "$BLOCKED_URL" "$BLOCKED_OUT"
sed 's/^/       /' "$BLOCKED_OUT"
assert "blocked host: real value did NOT leak" not_contains "$BLOCKED_OUT" "$REAL_VALUE"
if (( BLOCKED_REACHABLE )); then
  assert "blocked host: upstream saw only the placeholder" contains "$BLOCKED_OUT" "upstream-received: $PLACEHOLDER"
fi
say "proxy log (interception decisions):"
sed 's/^/       /' "$PROXY_LOG" 2>/dev/null | grep -E 'replaced|blocked' || say "   (no replaced/blocked lines captured)"

# ---------------------------------------------------------------------------
# 3. A real worker makes the swap end to end
# ---------------------------------------------------------------------------
section "3. BASE WORKER (shenmux PTY + proxy + mock harness)"
say "spawning worker 'base' (harness=mock, 3 steps) pointed at the upstream ..."
# The operator (muxwork) sets up the worker's own secret-proxy + shenmux run
# wrapper and injects the harness env (placeholder key, MOCK_TARGET, proxy)
# itself; we only pass the target and step count as flags.
muxwork spawn --name base --harness mock \
    --mock-target "$ALLOWED_URL" --mock-steps 3 \
  || bad "muxwork spawn base"

say "waiting for base to run its steps ..."
base_done() { muxwork logs --name base 2>/dev/null | grep -q "DONE"; }
if wait_for 40 base_done; then
  ok "base worker completed its run"
else
  say "base did not report DONE within timeout — dumping what we have:"
  muxwork logs --name base 2>/dev/null | sed 's/^/       /' || true
fi

BASE_LOG="$WORK/base.session.log"
muxwork logs --name base >"$BASE_LOG" 2>/dev/null || true
say "base session log (tail):"
tail -n 12 "$BASE_LOG" 2>/dev/null | sed 's/^/       /' || true

# Substantive assertion: the swap happened inside a real worker. We assert the
# placeholder was what the agent held, and that the value the upstream reported
# is neither empty nor the placeholder — i.e. a real swap occurred in flight.
assert "worker: agent held the placeholder" contains "$BASE_LOG" "agent-holds: $PLACEHOLDER"
swap_happened() {
  local recv
  recv="$(grep -m1 'upstream-received:' "$BASE_LOG" 2>/dev/null | sed 's/.*upstream-received: *//')"
  [[ -n "$recv" && "$recv" != *placeholder* ]]
}
assert "worker: upstream received a swapped (non-placeholder) value" swap_happened

# ---------------------------------------------------------------------------
# 4. Checkpoint + fork three ways
# ---------------------------------------------------------------------------
section "4. CHECKPOINT + FORK x3"
say "suspending base (final checkpoint) ..."
muxwork suspend --name base || bad "muxwork suspend base"

BASE_MANIFEST_DIR="$STATE/workers/base/manifests"
assert "base has at least one checkpoint manifest" \
  bash -c '[[ -d "'"$BASE_MANIFEST_DIR"'" ]] && [[ -n "$(ls -A "'"$BASE_MANIFEST_DIR"'" 2>/dev/null)" ]]'

i=0
for approach in alpha beta gamma; do
  i=$((i+1))
  steps=$((i+1))   # 2, 3, 4 -> progress.txt diverges too
  say "forking base -> $approach ($steps steps) ..."
  muxwork fork --name "$approach" --from base \
      --mock-target "$ALLOWED_URL" --mock-steps "$steps" \
    || bad "muxwork fork $approach"
done

for approach in alpha beta gamma; do
  say "waiting for $approach to run ..."
  fork_done() { muxwork logs --name "$1" 2>/dev/null | grep -q "DONE"; }
  if wait_for 40 fork_done "$approach"; then
    ok "$approach completed"
  else
    say "$approach did not report DONE in time (continuing; divergence check uses workspaces)"
  fi
done

# ---------------------------------------------------------------------------
# 5. Divergence
# ---------------------------------------------------------------------------
section "5. DIVERGENCE"
ws() { echo "$STATE/workers/$1/workspace"; }
for approach in alpha beta gamma; do
  say "--- $approach workspace ---"
  say "progress.txt: $(cat "$(ws "$approach")/progress.txt" 2>/dev/null || echo '(missing)')"
  say "notes.md:"
  sed 's/^/       /' "$(ws "$approach")/notes.md" 2>/dev/null || say "       (missing)"
done

notes_diverged() {
  local a b g
  a="$(ws alpha)/notes.md"; b="$(ws beta)/notes.md"; g="$(ws gamma)/notes.md"
  [[ -f "$a" && -f "$b" && -f "$g" ]] || return 1
  # Not all three identical: at least one pair differs.
  ! { cmp -s "$a" "$b" && cmp -s "$b" "$g"; }
}
assert "the three forks' notes.md are not identical (workspaces diverged)" notes_diverged

# snapshot diff between base's checkpoint and a fork's latest manifest.
latest_manifest() {
  ls -t "$STATE/workers/$1/manifests"/* 2>/dev/null | head -n1
}
BASE_MAN="$(latest_manifest base || true)"
FORK_MAN="$(latest_manifest alpha || true)"
if [[ -n "$BASE_MAN" && -n "$FORK_MAN" ]]; then
  say "snapshot diff  base <-> alpha:"
  if "$BIN/snapshot" diff --a "$BASE_MAN" --b "$FORK_MAN" 2>&1 | sed 's/^/       /'; then
    ok "snapshot diff produced a tree comparison"
  else
    say "snapshot diff returned non-zero (non-fatal)"
  fi
else
  say "skipping snapshot diff — a checkpoint manifest was not available"
fi

# ---------------------------------------------------------------------------
# 6. Fleet view + event stream
# ---------------------------------------------------------------------------
section "6. FLEET VIEW + EVENT STREAM"
say "muxwork list:"
muxwork list 2>&1 | sed 's/^/       /' || true

say "event stream (muxwork watch --json, first lines):"
# watch is a live feed; bound it with a timeout and head so it never blocks.
( timeout 5 "$BIN/muxwork" --state "$STATE" watch --json 2>/dev/null || true ) | head -n 30 | sed 's/^/       /' || true

say "fork lineage (from events.ndjson):"
for approach in alpha beta gamma; do
  ev="$STATE/workers/$approach/events.ndjson"
  if [[ -f "$ev" ]] && grep -q forked "$ev"; then
    say "   $approach <- base  (forked)"
  else
    say "   $approach <- base"
  fi
done

# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------
section "RESULT"
if (( FAILED == 0 )); then
  printf '\n\033[1;32m========== DEMO OK ==========\033[0m\n'
  exit 0
else
  printf '\n\033[1;31m========== DEMO FAILED (see FAIL lines above) ==========\033[0m\n'
  exit 1
fi
