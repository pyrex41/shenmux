#!/usr/bin/env bash
#
# demo.sh — bring the whole local + controller demo up with one command.
#
# Topology (four processes, all on loopback, all started by this script):
#
#   shenmux run          owns the PTY for session $SESSION and binds two IPC
#                        sockets ($RUN_DIR/$SESSION.{ctl,pub}). Writes a durable
#                        history checkpoint into $RUN_DIR/history.
#          |
#          |  ipc://
#          v
#   shenmux web          the RICH local browser gateway (internal/webui, PixiJS
#                        terminal: real keyboard input, resize, colour). This is
#                        the UI a human should open.
#
#   shenmux controller   the relay/controller with --dev-browser-subject. Serves
#                        /enroll, /sessions, /capabilities and /browser. No UI.
#          ^
#          |  outbound websocket (ws://.../ws)
#          |
#   shenmux agent        dials the controller, presents the enrolled device
#                        credential, and bridges controller streams to the IPC
#                        sockets above.
#
# Enrollment is automatic: the controller prints a single-use code to its
# stderr, this script reads it out of the log and feeds it to `shenmux login`.
# Nobody copy-pastes a code.
#
# Everything the demo owns (state, config, sockets, logs, pids) lives under
# $RUN_DIR, which is wiped and recreated on every run, so the demo never
# touches ~/.config/shenmux or ~/.local/state/shenmux.
#
# macOS-safe: bash 3.2, no `timeout`, no GNU-only flags.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_DIR"

SESSION="demo"
SUBJECT="local-demo"
PREFERRED_WEB_PORT=8787
PREFERRED_CONTROLLER_PORT=8788
RUN_DIR="${SHENMUX_DEMO_DIR:-/tmp/shenmux-demo-$(id -u)}"
MODE="up"          # up | detach | stop
REBUILD=0
READY_TIMEOUT=30   # seconds to wait for any one service

usage() {
  cat <<EOF
usage: scripts/demo.sh [flags]

  --session NAME         session name (default: $SESSION)
  --subject NAME         dev browser subject (default: $SUBJECT)
  --web-port N           preferred local gateway port (default: $PREFERRED_WEB_PORT)
  --controller-port N    preferred controller port (default: $PREFERRED_CONTROLLER_PORT)
  --run-dir DIR          demo scratch dir (default: $RUN_DIR)
  --detach               start everything, print the summary, and exit
  --stop                 stop a previous --detach run and exit
  --rebuild              rebuild bin/shenmux even if it looks current
  -h, --help             this message

Without --detach the script stays in the foreground; Ctrl-C stops everything.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --session) SESSION="$2"; shift 2 ;;
    --session=*) SESSION="${1#*=}"; shift ;;
    --subject) SUBJECT="$2"; shift 2 ;;
    --subject=*) SUBJECT="${1#*=}"; shift ;;
    --web-port) PREFERRED_WEB_PORT="$2"; shift 2 ;;
    --web-port=*) PREFERRED_WEB_PORT="${1#*=}"; shift ;;
    --controller-port) PREFERRED_CONTROLLER_PORT="$2"; shift 2 ;;
    --controller-port=*) PREFERRED_CONTROLLER_PORT="${1#*=}"; shift ;;
    --run-dir) RUN_DIR="$2"; shift 2 ;;
    --run-dir=*) RUN_DIR="${1#*=}"; shift ;;
    --detach) MODE="detach"; shift ;;
    --stop) MODE="stop"; shift ;;
    --rebuild) REBUILD=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) echo "demo.sh: unknown flag $1" >&2; usage >&2; exit 2 ;;
  esac
done

BIN="$REPO_DIR/bin/shenmux"
LOG_DIR="$RUN_DIR/logs"
PID_DIR="$RUN_DIR/pids"
HISTORY_DIR="$RUN_DIR/history"
CONTROL_ENDPOINT="ipc://$RUN_DIR/$SESSION.ctl"
DATA_ENDPOINT="ipc://$RUN_DIR/$SESSION.pub"

# Every shenmux process the demo starts reads its config/state from here, so a
# demo run can never disturb the operator's real enrollment.
export SHENMUX_CONFIG_FILE="$RUN_DIR/config.json"
export SHENMUX_STATE_DIR="$RUN_DIR/state"

# The services we manage, in start order. Also the cleanup order (reversed).
SERVICES="session controller agent web"

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
say()  { printf '   %s\n' "$*"; }
step() { printf '\n\033[1;36m==> %s\033[0m\n' "$*"; }
warn() { printf '   \033[1;33m!\033[0m %s\n' "$*"; }
ok()   { printf '   \033[1;32mok\033[0m %s\n' "$*"; }
die()  { printf '\n\033[1;31mdemo failed:\033[0m %s\n' "$*" >&2; exit 1; }

# ---------------------------------------------------------------------------
# Process bookkeeping. One pid file per service under $PID_DIR.
# ---------------------------------------------------------------------------
pid_file() { echo "$PID_DIR/$1.pid"; }
log_file() { echo "$LOG_DIR/$1.log"; }

record_pid() { mkdir -p "$PID_DIR"; echo "$2" >"$(pid_file "$1")"; }

read_pid() {
  local file
  file="$(pid_file "$1")"
  [ -f "$file" ] || return 1
  local pid
  pid="$(cat "$file" 2>/dev/null || true)"
  case "$pid" in
    ''|*[!0-9]*) return 1 ;;
  esac
  echo "$pid"
}

# owned_by_demo guards against killing an unrelated process that happens to
# have inherited a recycled pid: it must still look like one of our binaries.
owned_by_demo() {
  kill -0 "$1" 2>/dev/null || return 1
  ps -o command= -p "$1" 2>/dev/null | grep -q 'shenmux' || return 1
  return 0
}

# kill_tree stops a pid and any children it left behind (a PTY session leaves a
# login shell that must not outlive the demo).
kill_tree() {
  local pid="$1" kids
  kids="$(pgrep -P "$pid" 2>/dev/null || true)"
  kill "$pid" 2>/dev/null || true
  local waited=0
  while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt 50 ]; do
    sleep 0.1
    waited=$((waited + 1))
  done
  kill -9 "$pid" 2>/dev/null || true
  # Reap it so the shell does not print an asynchronous "Killed" notice later.
  wait "$pid" 2>/dev/null || true
  local kid
  for kid in $kids; do
    kill -9 "$kid" 2>/dev/null || true
  done
}

stop_service() {
  local name="$1" pid
  pid="$(read_pid "$name" || true)"
  if [ -n "$pid" ] && owned_by_demo "$pid"; then
    say "stopping $name (pid $pid)"
    kill_tree "$pid"
  fi
  rm -f "$(pid_file "$name")"
}

# Stop every service we have a pid file for, in reverse start order.
stop_all() {
  local name reversed=""
  for name in $SERVICES; do
    reversed="$name $reversed"
  done
  for name in $reversed; do
    stop_service "$name"
  done
}

CLEANUP_ARMED=1
cleanup() {
  local code=$?
  if [ "$CLEANUP_ARMED" -eq 1 ]; then
    CLEANUP_ARMED=0   # cleanup runs exactly once
    step "STOPPING"
    stop_all
    say "all demo processes stopped"
  fi
  exit "$code"
}
# The signal handlers only exit; the EXIT trap does the single cleanup pass.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---------------------------------------------------------------------------
# Ports
# ---------------------------------------------------------------------------
port_holder() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | awk 'NR==2 {print $1}'
}

# Ports already handed out by this run. A port we picked a moment ago is not
# yet bound, so probing alone would hand the same port out twice.
CLAIMED_PORTS=""

port_claimed() {
  local claimed
  for claimed in $CLAIMED_PORTS; do
    [ "$claimed" = "$1" ] && return 0
  done
  return 1
}

port_in_use() {
  port_claimed "$1" && return 0
  if command -v lsof >/dev/null 2>&1; then
    lsof -nP -iTCP:"$1" -sTCP:LISTEN >/dev/null 2>&1 && return 0
  fi
  # Fallback probe: a successful connect means somebody is listening.
  if (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; then
    exec 3>&- 2>/dev/null || true
    return 0
  fi
  return 1
}

# pick_port sets PICKED_PORT to the preferred port, or the next free one, and
# always says out loud when it had to move. Never fails silently.
PICKED_PORT=""
pick_port() {
  local preferred="$1" label="$2" holder
  local port="$preferred"
  local limit=$(( preferred + 40 ))
  while [ "$port" -le "$limit" ]; do
    if port_claimed "$port"; then
      warn "port $port is already claimed by this demo run — not using it for the $label"
    elif port_in_use "$port"; then
      holder="$(port_holder "$port")"
      warn "port $port is taken${holder:+ by $holder} — not using it for the $label"
    else
      if [ "$port" -ne "$preferred" ]; then
        say "$label moved to port $port (preferred $preferred was busy)"
      fi
      PICKED_PORT="$port"
      CLAIMED_PORTS="$CLAIMED_PORTS $port"
      return 0
    fi
    port=$((port + 1))
  done
  die "no free port for the $label between $preferred and $limit"
}

# ---------------------------------------------------------------------------
# Readiness: poll, never sleep-and-hope.
# ---------------------------------------------------------------------------
GET() { curl -fsS --max-time 3 --noproxy '*' "$@"; }

# wait_for <label> <seconds> <command...>
wait_for() {
  local label="$1" timeout="$2"
  shift 2
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if "$@" >/dev/null 2>&1; then
      ok "$label"
      return 0
    fi
    sleep 0.2
  done
  return 1
}

# fail_with_log aborts and shows why, using the service's own output.
fail_with_log() {
  local name="$1" message="$2"
  printf '\n\033[1;31mdemo failed:\033[0m %s\n' "$message" >&2
  printf 'last lines of %s:\n' "$(log_file "$name")" >&2
  tail -n 20 "$(log_file "$name")" 2>/dev/null >&2 || true
  exit 1
}

start_service() {
  local name="$1"
  shift
  mkdir -p "$LOG_DIR" "$PID_DIR"
  "$@" >"$(log_file "$name")" 2>&1 </dev/null &
  record_pid "$name" "$!"
  say "$name started (pid $!) — log: $(log_file "$name")"
}

# ---------------------------------------------------------------------------
# --stop: shut down a previous --detach run and leave.
# ---------------------------------------------------------------------------
if [ "$MODE" = "stop" ]; then
  CLEANUP_ARMED=0
  step "STOPPING (run dir $RUN_DIR)"
  if [ -d "$PID_DIR" ]; then
    stop_all
    ok "demo stopped"
  else
    say "nothing to stop — no pid files under $PID_DIR"
  fi
  exit 0
fi

# ---------------------------------------------------------------------------
# 0. Clear our own leftovers, then start from a clean run dir.
# ---------------------------------------------------------------------------
step "0. RUN DIR  $RUN_DIR"
if [ -d "$PID_DIR" ]; then
  CLEANUP_ARMED=0   # these pids belong to the previous run, not to this one
  say "found a previous run — clearing it"
  stop_all
  CLEANUP_ARMED=1
fi
rm -rf "$RUN_DIR"
mkdir -p "$RUN_DIR" "$LOG_DIR" "$PID_DIR" "$HISTORY_DIR" "$SHENMUX_STATE_DIR"
# The daemon refuses to bind IPC sockets in a directory with group/other access.
chmod 700 "$RUN_DIR" "$SHENMUX_STATE_DIR"
ok "clean run dir ready"

# ---------------------------------------------------------------------------
# 1. Build. Only bin/shenmux is needed; the browser assets are committed under
#    internal/webui, so no npm step is required for the demo.
# ---------------------------------------------------------------------------
step "1. BUILD"
needs_build=0
if [ ! -x "$BIN" ] || [ "$REBUILD" -eq 1 ]; then
  needs_build=1
elif [ -n "$(find cmd internal client -name '*.go' -newer "$BIN" 2>/dev/null | head -n 1)" ]; then
  say "Go sources are newer than $BIN"
  needs_build=1
fi
if [ "$needs_build" -eq 1 ]; then
  say "go build -o bin/shenmux ./cmd/shenmux"
  mkdir -p "$REPO_DIR/bin"
  CGO_ENABLED=0 go build -o "$BIN" ./cmd/shenmux || die "go build failed"
fi
[ -x "$BIN" ] || die "bin/shenmux is missing after the build"
ok "bin/shenmux ($("$BIN" version))"

# ---------------------------------------------------------------------------
# 2. Ports
# ---------------------------------------------------------------------------
step "2. PORTS"
pick_port "$PREFERRED_CONTROLLER_PORT" "controller"; CONTROLLER_PORT="$PICKED_PORT"
pick_port "$PREFERRED_WEB_PORT" "local web gateway";  WEB_PORT="$PICKED_PORT"
CONTROLLER_URL="http://127.0.0.1:$CONTROLLER_PORT"
WEB_URL="http://127.0.0.1:$WEB_PORT"
# The gateway requires an access token, so a stale tab or another local process
# cannot attach just by reaching the port. It generates one when not told;
# supplying it here means the demo knows the URL to print without parsing logs.
WEB_TOKEN="demo-$$-$(date +%s)"
WEB_OPEN_URL="$WEB_URL/?token=$WEB_TOKEN"
ok "controller $CONTROLLER_URL   local gateway $WEB_URL"

# ---------------------------------------------------------------------------
# 3. The PTY session
# ---------------------------------------------------------------------------
step "3. SESSION  ($SESSION)"
start_service session "$BIN" run \
  --session "$SESSION" \
  --control "$CONTROL_ENDPOINT" \
  --data "$DATA_ENDPOINT" \
  --history-dir "$HISTORY_DIR"
session_ready() { [ -S "$RUN_DIR/$SESSION.ctl" ] && [ -S "$RUN_DIR/$SESSION.pub" ]; }
wait_for "IPC sockets bound ($RUN_DIR/$SESSION.{ctl,pub})" "$READY_TIMEOUT" session_ready \
  || fail_with_log session "the session never bound its IPC sockets"

# ---------------------------------------------------------------------------
# 4. The controller
# ---------------------------------------------------------------------------
step "4. CONTROLLER  ($CONTROLLER_URL)"
start_service controller "$BIN" controller \
  --listen "127.0.0.1:$CONTROLLER_PORT" \
  --origin "$CONTROLLER_URL" \
  --dev-browser-subject "$SUBJECT" \
  --state-dir "$RUN_DIR/controller-state"
wait_for "controller answers /healthz" "$READY_TIMEOUT" GET "$CONTROLLER_URL/healthz" \
  || fail_with_log controller "the controller never answered /healthz"

# ---------------------------------------------------------------------------
# 5. Enrollment — read the one-time code out of the controller's own log.
# ---------------------------------------------------------------------------
step "5. ENROLLMENT"
extract_code() {
  sed -n 's/.*enrollment_codes=\([^,[:space:]]*\).*/\1/p' "$(log_file controller)" | head -n 1
}
code_present() { [ -n "$(extract_code)" ]; }
wait_for "controller printed a single-use enrollment code" 10 code_present \
  || fail_with_log controller "no enrollment code appeared in the controller log"
ENROLLMENT_CODE="$(extract_code)"
say "enrolling this device automatically (no copy-paste)"
"$BIN" login --controller "$CONTROLLER_URL" --code "$ENROLLMENT_CODE" >"$(log_file login)" 2>&1 \
  || fail_with_log login "shenmux login failed"
ok "$(grep 'device enrolled' "$(log_file login)" || echo 'device enrolled')"

# ---------------------------------------------------------------------------
# 6. The agent (outbound to the controller, bridged to the session's sockets)
# ---------------------------------------------------------------------------
step "6. AGENT"
start_service agent "$BIN" agent \
  --controller "$CONTROLLER_URL" \
  --transport relay \
  --session "$SESSION" \
  --label demo=true \
  --control "$CONTROL_ENDPOINT" \
  --data "$DATA_ENDPOINT"
agent_registered() {
  GET "$CONTROLLER_URL/sessions?subject=$SUBJECT" | grep -q "\"name\":\"$SESSION\""
}
wait_for "session '$SESSION' is visible in the controller's /sessions" "$READY_TIMEOUT" agent_registered \
  || fail_with_log agent "the agent never registered the session with the controller"

# ---------------------------------------------------------------------------
# 7. The rich local web gateway
# ---------------------------------------------------------------------------
step "7. LOCAL WEB GATEWAY  ($WEB_URL)"
start_service web "$BIN" web \
  --session "$SESSION" \
  --listen "127.0.0.1:$WEB_PORT" \
  --control "$CONTROL_ENDPOINT" \
  --data "$DATA_ENDPOINT" \
  --history-dir "$HISTORY_DIR" \
  --token "$WEB_TOKEN"
wait_for "gateway answers /healthz" "$READY_TIMEOUT" GET "$WEB_URL/healthz" \
  || fail_with_log web "the local gateway never answered /healthz"
wait_for "gateway serves the PixiJS client at /" "$READY_TIMEOUT" GET "$WEB_OPEN_URL" \
  || fail_with_log web "the local gateway never served its index page"
history_live() { GET "$WEB_URL/api/history?session=$SESSION&token=$WEB_TOKEN" | grep -q "\"session\":\"$SESSION\""; }
wait_for "history API has a checkpoint for '$SESSION'" "$READY_TIMEOUT" history_live \
  || fail_with_log web "the history API never returned a checkpoint"

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
STOP_HINT="scripts/demo.sh --stop"
[ "$MODE" = "detach" ] || STOP_HINT="press Ctrl-C here  (or from another terminal: scripts/demo.sh --stop)"

cat <<EOF

$(printf '\033[1;32m')================= shenmux demo is up =================$(printf '\033[0m')

  $(printf '\033[1;32mOPEN THIS')  ->  $WEB_OPEN_URL$(printf '\033[0m')
      The rich local terminal (PixiJS): real keyboard input, resize,
      colour, blinking cursor. This is the UI to judge the product by.

  Also running:
    $CONTROLLER_URL/sessions?subject=$SUBJECT
      Controller session inventory (JSON) — proves the agent is enrolled.
    $WEB_URL/api/history?session=$SESSION&token=$WEB_TOKEN
      Durable session history (JSON) served by the local gateway.

  Terminal client instead of a browser:
    ./bin/muxctl -session $SESSION -control $CONTROL_ENDPOINT -data $DATA_ENDPOINT

  session:  $SESSION
  run dir:  $RUN_DIR   (state, config, sockets, logs, pids)
  logs:     $LOG_DIR
  stop:     $STOP_HINT

EOF

if [ "$MODE" = "detach" ]; then
  CLEANUP_ARMED=0   # leave the services running for the next command
  exit 0
fi

# Foreground: hold the terminal, and exit (cleaning up) if anything dies.
step "RUNNING — Ctrl-C to stop everything"
while :; do
  sleep 1
  for name in $SERVICES; do
    pid="$(read_pid "$name" || true)"
    if [ -z "$pid" ] || ! kill -0 "$pid" 2>/dev/null; then
      warn "$name exited unexpectedly — see $(log_file "$name")"
      tail -n 10 "$(log_file "$name")" 2>/dev/null || true
      exit 1
    fi
  done
done
