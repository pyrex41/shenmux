#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v bifrost >/dev/null 2>&1; then
  echo "ERROR: bifrost is not on PATH" >&2
  exit 1
fi

# Bifrost discovers a project-local bifrost.suite.json from the current root.
exec bifrost "$@"
