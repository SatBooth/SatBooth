#!/usr/bin/env bash
# Installs the pinned supply-chain tools into .tools/bin. Needs the network. The checksum database
# stays on, so downloads are verified against sum.golang.org. Nothing is added to go.mod.
set -euo pipefail

cd "$(dirname "$0")/.."

export GOBIN="$PWD/.tools/bin"
export GOFLAGS=-mod=mod GOPROXY=https://proxy.golang.org GOSUMDB=sum.golang.org
# A caller's environment must not weaken checksum verification.
unset GONOSUMDB GONOSUMCHECK GOINSECURE GOPRIVATE GONOPROXY

go install golang.org/x/vuln/cmd/govulncheck@v1.9.0
go install github.com/google/capslock/cmd/capslock@v0.3.4
echo "tools.sh: OK"
