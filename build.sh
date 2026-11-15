#!/usr/bin/env bash
# Build single executables for Linux and Windows. Requires Go >= 1.26.
# Windows icon/version info: install go-winres once (go install github.com/tc-hib/go-winres@latest);
# without it the committed rsrc_windows_*.syso files are used as they are.
set -e
cd "$(dirname "$0")"
if git rev-parse --git-dir >/dev/null 2>&1; then
  BD=$(git log -1 --format=%cd --date=format:'%Y-%m-%d %H:%M:%S')
  TAG=$(git describe --tags --abbrev=0 2>/dev/null || echo v0.0.0)
else
  BD=$(date '+%Y-%m-%d %H:%M:%S')
  TAG=v0.0.0
fi
VER=${TAG#v}

WINRES=$(command -v go-winres || true)
[ -z "$WINRES" ] && [ -x "$(go env GOPATH)/bin/go-winres" ] && WINRES="$(go env GOPATH)/bin/go-winres"
if [ -n "$WINRES" ]; then
  "$WINRES" simply --arch amd64,arm64 --icon winres/icon.png --manifest cli \
    --product-name tinyHFS --file-description "tinyHFS - tiny HTTP file server" \
    --product-version "$VER" --file-version "$VER" \
    --original-filename tinyhfs.exe --copyright "sendust"
  echo "windows resources: icon + version $VER"
else
  echo "go-winres not found: using existing rsrc_windows_*.syso (if any)"
fi

LDF="-s -w -X 'main.buildDate=$BD'"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$LDF" -o dist/tinyhfs .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDF" -o dist/tinyhfs.exe .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$LDF" -o dist/tinyhfs-arm64 .
echo "built ($BD):"; ls -l dist
