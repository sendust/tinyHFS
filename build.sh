#!/usr/bin/env bash
# Build single executables for Linux and Windows (amd64). Requires Go >= 1.21.
set -e
cd "$(dirname "$0")"
if git rev-parse --git-dir >/dev/null 2>&1; then
  BD=$(git log -1 --format=%cd --date=format:'%Y-%m-%d %H:%M:%S')
else
  BD=$(date '+%Y-%m-%d %H:%M:%S')
fi
LDF="-s -w -X 'main.buildDate=$BD'"
mkdir -p dist
CGO_ENABLED=0 GOOS=linux   GOARCH=amd64 go build -trimpath -ldflags "$LDF" -o dist/tinyhfs .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags "$LDF" -o dist/tinyhfs.exe .
CGO_ENABLED=0 GOOS=linux   GOARCH=arm64 go build -trimpath -ldflags "$LDF" -o dist/tinyhfs-arm64 .
echo "built ($BD):"; ls -l dist
