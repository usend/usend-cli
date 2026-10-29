#!/usr/bin/env bash
# Builds the uSend CLI release archives into a directory:
#   usend_<os>_<arch>.tar.gz (macOS, Linux), usend_windows_<arch>.zip, and checksums.txt (sha256sum format)
# These are the names web/landing/install.sh and install.ps1 download. Static binaries (no cgo), so they run on
# any Linux, Alpine included.
#
#   scripts/build.sh 1.0.0 dist        (from the CLI module, as the public usend-cli repository runs it)
set -euo pipefail
version="${1:?usage: build-cli.sh <version> <out-dir>}"
out="${2:?usage: build-cli.sh <version> <out-dir>}"
version="${version#v}"
mod="$(cd "$(dirname "$0")/.." && pwd)"
mkdir -p "$out"
out="$(cd "$out" && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64 windows/arm64; do
  os="${target%/*}" arch="${target#*/}"
  bin="usend"
  [[ $os == windows ]] && bin="usend.exe"
  mkdir -p "$work/$os-$arch"
  (cd "$mod" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X usend.et/cli/internal/app.Version=$version" -o "$work/$os-$arch/$bin" ./cmd/usend)
  if [[ $os == windows ]]; then
    (cd "$work/$os-$arch" && zip -q -X "$out/usend_${os}_${arch}.zip" "$bin")
  else
    tar -C "$work/$os-$arch" -czf "$out/usend_${os}_${arch}.tar.gz" "$bin"
  fi
  echo "built usend_${os}_${arch}"
done

(cd "$out" && if command -v sha256sum >/dev/null; then sha256sum usend_*; else shasum -a 256 usend_*; fi) >"$out/checksums.txt"
echo "checksums.txt written to $out"
