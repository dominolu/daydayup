#!/usr/bin/env bash
# Build the macOS desktop shell. The gateway remains a normal Go package; the
# shell is a separate module because Wails v3 requires a newer Go toolchain.
set -euo pipefail
cd "$(dirname "$0")/.."

# The desktop module requires Go 1.26. Prefer a vendored toolchain: `go` 1.21+
# runs the one in <module>/vendor/go when its version satisfies go.mod, so an
# offline or sandboxed host (no toolchain download) can still build. Install it
# with scripts/install-go126.sh; otherwise fall back to the local Go and let
# GOTOOLCHAIN download the required version.
if [[ -x "$PWD/desktop/vendor/go/bin/go" ]]; then
  export PATH="$PWD/desktop/vendor/go/bin:$PATH"
else
  if ! command -v go >/dev/null 2>&1; then
    for c in /usr/local/go/bin/go /opt/homebrew/bin/go "$HOME/sdk/go1.23.12/bin/go"; do
      if [[ -x "$c" ]]; then export PATH="$(dirname "$c"):$PATH"; break; fi
    done
  fi
  command -v go >/dev/null 2>&1 || { echo "error: go toolchain not found (run scripts/install-go126.sh)" >&2; exit 1; }
fi

# Keep the desktop build's Go toolchain, module cache and build cache inside
# the repository so a restricted environment can still build.
export GOTOOLCHAIN=auto
export GOCACHE="$PWD/.gocache"
export GOPATH="$PWD/.tools/gopath"
export GOMODCACHE="$PWD/.tools/gopath/pkg/mod"
export GOFLAGS="${GOFLAGS:--mod=mod}"
export CGO_ENABLED=1

mkdir -p bin
cd desktop
go build -tags production -trimpath -o ../bin/safuno ./cmd/safuno
echo "built bin/safuno"
