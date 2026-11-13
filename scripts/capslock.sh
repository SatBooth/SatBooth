#!/usr/bin/env bash
# Usage: scripts/capslock.sh [update]
#   (default)  compare the current capabilities with docs/capslock.json; fail if any was added.
#   update     rewrite docs/capslock.json (review the diff: a new capability needs a reason in the PR).
# Offline once scripts/tools.sh has run.
set -euo pipefail

cd "$(dirname "$0")/.."

export GOFLAGS=-mod=vendor GOPROXY=off CGO_ENABLED=0
bin=.tools/bin/capslock
# Fixed platform (the release target) so the baseline does not depend on the host.
plat=(-goos linux -goarch amd64)
[ -x "$bin" ] || { echo "capslock.sh: run scripts/tools.sh first" >&2; exit 1; }

case "${1:-compare}" in
  update)
    tmp=$(mktemp)
    trap 'rm -f "$tmp"' EXIT
    "$bin" "${plat[@]}" -packages ./... -output=json > "$tmp"
    chmod 644 "$tmp"
    mv "$tmp" docs/capslock.json
    trap - EXIT
    echo "capslock.sh: wrote docs/capslock.json"
    ;;
  compare)
    out=$(mktemp)
    trap 'rm -f "$out"' EXIT
    status=0
    "$bin" "${plat[@]}" -packages ./... -output=compare docs/capslock.json > "$out" 2>&1 || status=$?
    cat "$out"
    # Verified with capslock v0.3.4: compare exits 1 when a capability is added to a package
    # ("has new capability ... compared to the baseline") and 0 when nothing changed.
    if [ "$status" -ne 0 ]; then
      echo "capslock.sh: FAIL: capabilities changed or capslock failed (exit $status); if intended, run scripts/capslock.sh update and justify it in the PR" >&2
      exit 1
    fi
    # The whole-module baseline lists NETWORK for the package setup: capslock resolves an io.Writer call inside
    # encoding/json to every writer type in the program, net/http's among them (docs/dependencies.md). The two
    # commands of the tools image run with no network, so each is also analysed ALONE, where that over-approximation
    # cannot arise, and must have none. Fails closed: capslock failing, printing nothing, or printing no "capability"
    # field all fail.
    alone=$(mktemp)
    trap 'rm -f "$out" "$alone"' EXIT
    for cmd in satbooth-setup satbooth-pair; do
      "$bin" "${plat[@]}" -packages "./cmd/$cmd" -output=json > "$alone" \
        || { echo "capslock.sh: FAIL: capslock failed on ./cmd/$cmd" >&2; exit 1; }
      grep -q '"capability":' "$alone" \
        || { echo "capslock.sh: FAIL: capslock printed no capabilities for ./cmd/$cmd (layout changed?)" >&2; exit 1; }
      if grep -q '"capability": *"CAPABILITY_NETWORK"' "$alone"; then
        echo "capslock.sh: FAIL: $cmd has a network capability" >&2
        exit 1
      fi
    done
    echo "capslock.sh: OK"
    ;;
  *)
    echo "usage: scripts/capslock.sh [update]" >&2
    exit 2
    ;;
esac
