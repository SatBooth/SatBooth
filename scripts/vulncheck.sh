#!/usr/bin/env bash
# Runs govulncheck over the module. Needs the network (vulnerability database). Run scripts/tools.sh first.
# Exits non-zero on any finding.
set -euo pipefail

cd "$(dirname "$0")/.."

[ -x .tools/bin/govulncheck ] || { echo "vulncheck.sh: run scripts/tools.sh first" >&2; exit 1; }

GOFLAGS=-mod=vendor CGO_ENABLED=0 .tools/bin/govulncheck ./...
echo "vulncheck.sh: OK"
