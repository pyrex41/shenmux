#!/usr/bin/env bash
#
# internal/webui/app.bundle.js is a committed build artifact: it is what
# files.go embeds and what the browser actually loads. Editing pixi-client.js
# without rebuilding ships a page with none of the change, and nothing else in
# the tree notices. This is the same discipline scripts/shenguard-audit.sh
# applies to generated Go.
#
# esbuild's output is byte-identical for identical inputs and a pinned esbuild
# (see web/package.json and web/package-lock.json), so a plain diff is safe.
# What it therefore catches: a stale bundle, on a machine whose node_modules
# match the lockfile. What it does not catch: drift caused by node_modules that
# disagree with the lockfile, which shows up here as a spurious failure rather
# than a silent pass.
set -euo pipefail
cd "$(dirname "$0")/.."

bundle="internal/webui/app.bundle.js"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

npm run --silent build --prefix web -- --outfile "$work/app.bundle.js"

if ! cmp -s "$work/app.bundle.js" "$bundle"; then
  echo "FAIL: $bundle does not match a fresh build of internal/webui/pixi-client.js" >&2
  echo "      the browser is loading the committed bundle, so the change is not shipping" >&2
  echo "      run: npm run build --prefix web" >&2
  exit 1
fi

echo "PASS: $bundle matches its sources"
