#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
version=${1:?Usage: package.sh VERSION}
version=${version#v}
mkdir -p dist/release
for os in darwin linux; do
  for arch in amd64 arm64; do
    stage=$(mktemp -d)
    CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$stage/plane" .
    cp README.md "$stage/"
    cp ../../LICENSE.txt "$stage/LICENSE"
    tar -czf "dist/release/plane_${version}_${os}_${arch}.tar.gz" -C "$stage" plane README.md LICENSE
    rm -rf "$stage"
  done
done
cd dist/release
sha256sum plane_*.tar.gz > SHA256SUMS
