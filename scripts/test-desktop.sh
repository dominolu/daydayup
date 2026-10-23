#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
  for c in /usr/local/go/bin/go /opt/homebrew/bin/go "$HOME/sdk/go1.23.12/bin/go"; do
    if [[ -x "$c" ]]; then export PATH="$(dirname "$c"):$PATH"; break; fi
  done
fi
command -v go >/dev/null 2>&1 || { echo "error: go toolchain not found"; exit 1; }

export GOTOOLCHAIN=auto
export GOCACHE="$PWD/.gocache"
export GOPATH="$PWD/.tools/gopath"
export GOMODCACHE="$PWD/.tools/gopath/pkg/mod"
export GOFLAGS="${GOFLAGS:--mod=mod}"
export CGO_ENABLED=1

cd desktop
go test ./...
