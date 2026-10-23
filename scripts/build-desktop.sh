#!/usr/bin/env bash
# Build the macOS desktop shell. The gateway remains a normal Go package; the
# shell is a separate module because Wails v3 requires a newer Go toolchain.
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
  for c in /usr/local/go/bin/go /opt/homebrew/bin/go "$HOME/sdk/go1.23.12/bin/go"; do
    if [[ -x "$c" ]]; then export PATH="$(dirname "$c"):$PATH"; break; fi
  done
fi
command -v go >/dev/null 2>&1 || { echo "error: go toolchain not found"; exit 1; }

# Keep the desktop build's Go 1.26+ toolchain, module cache and build cache
# inside the repository so a restricted environment can still build.
export GOTOOLCHAIN=auto
export GOCACHE="$PWD/.gocache"
export GOPATH="$PWD/.tools/gopath"
export GOMODCACHE="$PWD/.tools/gopath/pkg/mod"
export GOFLAGS="${GOFLAGS:--mod=mod}"
export CGO_ENABLED=1

mkdir -p bin
cd desktop
go build -tags production -trimpath -o ../bin/daydayup ./cmd/daydayup
echo "built bin/daydayup"
