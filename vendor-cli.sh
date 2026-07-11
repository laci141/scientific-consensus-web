#!/usr/bin/env bash
# vendor-cli.sh — cross-compile the scientific-consensus CLI to a linux/amd64
# binary at bin/scientific-consensus-pp-cli-linux, which the Dockerfile copies
# into the runtime image.
#
# WHY: a Windows .exe can't run in the Linux container, and the CLI lives on an
# unmerged monorepo branch (so `go install github.com/...` won't resolve). This
# vendors the CLI Go source into ./cli-src (build scratch, gitignored) and builds
# the linux binary from it.
#
# USAGE (from WEB_DIR, in Git Bash), with the monorepo on the scientific-consensus
# branch (its working tree must contain go.mod + cmd/ + internal/):
#   ./vendor-cli.sh
#   ./vendor-cli.sh "/c/Users/LACI/Desktop/printing-press-library/library/other/scientific-consensus"
#
# Then:  git add bin/scientific-consensus-pp-cli-linux && docker build -t app .
set -euo pipefail

CLI_SRC="${1:-/c/Users/LACI/Desktop/printing-press-library/library/other/scientific-consensus}"
OUT="bin/scientific-consensus-pp-cli-linux"

if [ ! -f "$CLI_SRC/go.mod" ] || [ ! -d "$CLI_SRC/cmd" ]; then
  echo "ERROR: CLI source not found at: $CLI_SRC" >&2
  echo "       Check out the monorepo on the scientific-consensus branch first" >&2
  echo "       (the working tree must actually contain go.mod + cmd/ + internal/)." >&2
  exit 1
fi

echo "Vendoring CLI Go source from: $CLI_SRC"
rm -rf cli-src && mkdir -p cli-src
cp "$CLI_SRC/go.mod" "$CLI_SRC/go.sum" cli-src/
cp -r "$CLI_SRC/cmd" "$CLI_SRC/internal" cli-src/

echo "Cross-compiling linux/amd64 -> $OUT"
mkdir -p bin
( cd cli-src && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath \
    -o "../$OUT" ./cmd/scientific-consensus-pp-cli )

echo "OK:"
file "$OUT"
ls -la "$OUT"
