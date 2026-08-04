#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

./scripts/shengen-codegen.sh --check

unexpected="$(find internal/shenguard -maxdepth 1 -type f \
  ! -name 'guards_gen.go' ! -name 'guards_test.go' \
  ! -name 'reducer.go' ! -name 'reducer_test.go' -print)"
if [[ -n "$unexpected" ]]; then
  echo "FAIL: unexpected files in the trusted shenguard package:" >&2
  echo "$unexpected" >&2
  exit 1
fi

if grep -R --line-number --include='*.go' -E '(^|[^[:alnum:]_])shenguard\.Session\{' \
  --exclude='guards_gen.go' --exclude='guards_test.go' .; then
  echo "FAIL: raw shenguard.Session literal found outside the guard package" >&2
  exit 1
fi

echo "PASS: shenguard TCB is closed and generated output is current"
