#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

args=(-spec specs/mux.shen -template codegen/guards_gen.go.tmpl -out internal/shenguard/guards_gen.go)
if [[ "${1:-}" == "--check" ]]; then
  args+=(-check)
  shift
fi
go run ./cmd/shenmux-gen "${args[@]}"

# Optional compatibility parse through upstream Shen-Backpressure. Its generic
# emitter intentionally does not replace the mux-specific uint16/uint64 host
# representation yet; this verifies only that it accepts the Shen surface.
if [[ "${UPSTREAM_SHENGEN_AUDIT:-0}" == "1" ]]; then
  shengen_bin="${SHENGEN_BIN:-shengen}"
  "$shengen_bin" specs/mux.shen shenguard >/dev/null
  echo "PASS: upstream shengen accepted specs/mux.shen"
fi
