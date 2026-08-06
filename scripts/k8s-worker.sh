#!/usr/bin/env bash
#
# k8s-worker.sh — add or remove a shenmux worker in the local k3s trial.
#
# A worker is one pod: a PTY owned by `shenmux run`, the browser gateway that
# serves it, and an agent that dials the controller outbound. Each worker needs
# its own session name and its own enrollment code, because codes are
# single-use — which is why scaling the Deployment replicas does not work: the
# second pod would find the code already spent.
#
#   scripts/k8s-worker.sh add alpha        # create worker "alpha"
#   scripts/k8s-worker.sh list             # what is running
#   scripts/k8s-worker.sh open alpha       # port-forward its terminal, print URL
#   scripts/k8s-worker.sh rm alpha         # delete it
#
# Requires the controller from deploy/kubernetes/local-trial.yaml to be running.
set -euo pipefail

NS=shenmux
IMAGE=shenmux:local
TOKEN=local-trial

die() { printf 'k8s-worker: %s\n' "$*" >&2; exit 1; }
say() { printf '  %s\n' "$*"; }

need_controller() {
  kubectl -n "$NS" get deploy/shenmux-controller >/dev/null 2>&1 \
    || die "controller not found; kubectl apply -f deploy/kubernetes/local-trial.yaml first"
}

# A fresh single-use code. The controller prints a batch at startup and spends
# them one at a time, so ask it to print more rather than reusing one.
fresh_code() {
  local used_by n code
  n=$(kubectl -n "$NS" get deploy -l shenmux/worker --no-headers 2>/dev/null | wc -l | tr -d ' ')
  # Restarting the controller reissues codes; it also drops the enrollment of
  # every existing agent, so only do it when nothing is enrolled yet.
  if [ "$n" -gt 0 ]; then
    kubectl -n "$NS" get secret shenmux-codes -o jsonpath='{.data.spare}' 2>/dev/null \
      | base64 -d 2>/dev/null | tr ' ' '\n' | grep -v '^$' | head -1 && return 0
  fi
  kubectl -n "$NS" logs deploy/shenmux-controller 2>/dev/null \
    | sed -n 's/.*enrollment_codes=\([^ ]*\).*/\1/p' | tail -1
}

cmd_add() {
  local name="${1:-}"
  [ -n "$name" ] || die "usage: $0 add NAME"
  need_controller

  local code
  code=$(fresh_code)
  [ -n "$code" ] || die "no enrollment code available; restart the controller with --enrollment-count N"

  kubectl -n "$NS" create secret generic "shenmux-enroll-$name" \
    --from-literal=code="$code" --dry-run=client -o yaml | kubectl apply -f - >/dev/null

  sed -e "s/__NAME__/$name/g" -e "s|__IMAGE__|$IMAGE|g" -e "s/__TOKEN__/$TOKEN/g" \
    "$(dirname "$0")/../deploy/kubernetes/worker-template.yaml" | kubectl apply -f - >/dev/null

  say "worker '$name' created; waiting for it to come up..."
  kubectl -n "$NS" rollout status "deploy/shenmux-worker-$name" --timeout=180s >/dev/null \
    || die "worker '$name' did not become ready; kubectl -n $NS logs deploy/shenmux-worker-$name -c agent"
  say "worker '$name' ready"
  say "open it with: $0 open $name"
}

cmd_rm() {
  local name="${1:-}"
  [ -n "$name" ] || die "usage: $0 rm NAME"
  kubectl -n "$NS" delete deploy "shenmux-worker-$name" --ignore-not-found >/dev/null
  kubectl -n "$NS" delete svc "shenmux-web-$name" --ignore-not-found >/dev/null
  kubectl -n "$NS" delete secret "shenmux-enroll-$name" --ignore-not-found >/dev/null
  say "worker '$name' removed"
}

cmd_list() {
  need_controller
  printf '\nWorkers:\n'
  kubectl -n "$NS" get deploy -l shenmux/worker \
    -o custom-columns=NAME:.metadata.labels.shenmux/worker,READY:.status.readyReplicas,AGE:.metadata.creationTimestamp \
    --no-headers 2>/dev/null | sed 's/^/  /' || say "(none)"
  printf '\nAdvertised to the controller:\n'
  curl -s -H 'X-Shenmux-Subject: local-test' http://127.0.0.1:18788/sessions 2>/dev/null \
    | tr ',' '\n' | grep -o '"name":"[^"]*"' | sed 's/.*:"/  /; s/"//' | sort -u || say "  (controller unreachable)"
  printf '\n'
}

cmd_open() {
  local name="${1:-}"
  [ -n "$name" ] || die "usage: $0 open NAME"
  local port
  port=$(( 8800 + $(printf '%s' "$name" | cksum | cut -d' ' -f1) % 100 ))
  say "forwarding svc/shenmux-web-$name to 127.0.0.1:$port (ctrl-c to stop)"
  say "OPEN: http://127.0.0.1:$port/?token=$TOKEN"
  kubectl -n "$NS" port-forward "svc/shenmux-web-$name" "$port:8787"
}

case "${1:-}" in
  add)  shift; cmd_add "$@" ;;
  rm)   shift; cmd_rm "$@" ;;
  list) shift; cmd_list "$@" ;;
  open) shift; cmd_open "$@" ;;
  *)    sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 1 ;;
esac
