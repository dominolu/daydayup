#!/usr/bin/env bash
# Install the Go toolchain the desktop module needs (go.mod: go 1.26.0) into
# desktop/vendor/go, where `go` finds it automatically and fully offline.
#
# Needed on hosts where GOTOOLCHAIN=auto cannot download a toolchain (for
# example a sandbox that cannot write ~/go/pkg/sumdb) or when offline.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${1:-1.26.0}"
GOOS="${GOOS:-$(go env GOOS 2>/dev/null || uname -s | tr '[:upper:]' '[:lower:]')}"
GOARCH="${GOARCH:-$(go env GOARCH 2>/dev/null || uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')}"
FILE="go${VERSION}.${GOOS}-${GOARCH}.tar.gz"
DEST="desktop/vendor/go"

if [[ -x "$DEST/bin/go" ]] && "$DEST/bin/go" version | grep -q " go${VERSION} "; then
  echo "already installed: $($DEST/bin/go version)"
  exit 0
fi

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "downloading $FILE ..."
curl -fsSL --max-time 600 -o "$TMP/$FILE" "https://go.dev/dl/$FILE"
EXPECTED="$(curl -fsSL --max-time 60 'https://go.dev/dl/?mode=json&include=all' | python3 -c '
import json, sys
want = sys.argv[1]
for rel in json.load(sys.stdin):
    for f in rel["files"]:
        if f["filename"] == want:
            print(f["sha256"]); raise SystemExit(0)
raise SystemExit("checksum not published for " + want)
' "$FILE")"
ACTUAL="$(shasum -a 256 "$TMP/$FILE" | cut -d' ' -f1)"
[[ "$ACTUAL" == "$EXPECTED" ]] || { echo "error: checksum mismatch for $FILE" >&2; exit 1; }
echo "checksum ok: $ACTUAL"
mkdir -p "$TMP/x" desktop/vendor
tar -xzf "$TMP/$FILE" -C "$TMP/x"
rm -rf "$DEST"
mv "$TMP/x/go" "$DEST"
"$DEST/bin/go" version
