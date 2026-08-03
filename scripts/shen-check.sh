#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if command -v bifrost >/dev/null 2>&1; then
  exec bifrost run tests/mux-spec.shen --impl "${SHEN_IMPL:-shen-go}"
fi

if [[ -n "${SHEN_BIN:-}" ]]; then
  exec "$SHEN_BIN" script tests/mux-spec.shen
fi

for candidate in shen-go shen; do
  if command -v "$candidate" >/dev/null 2>&1; then
    exec "$candidate" script tests/mux-spec.shen
  fi
done

echo "ERROR: no Bifrost or Shen launcher found." >&2
echo "Set SHEN_BIN=/path/to/shen-go or install pyrex41/bifrost." >&2
exit 1
