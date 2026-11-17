#!/usr/bin/env bash
# Download a published CLI, verify SHA-256, and install without root.
set -euo pipefail
repo=NorthLin/plane
version=${PLANE_VERSION:-latest}
case "$(uname -s)" in Darwin) os=darwin ;; Linux) os=linux ;; *) echo 'Supported systems: macOS and Linux' >&2; exit 1 ;; esac
case "$(uname -m)" in x86_64|amd64) arch=amd64 ;; arm64|aarch64) arch=arm64 ;; *) echo 'Supported architectures: amd64 and arm64' >&2; exit 1 ;; esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
if [ "$version" = latest ]; then
  version=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest" | sed -n 's/.*"tag_name": "\([^"]*\)".*/\1/p')
fi
[[ "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][a-zA-Z0-9.-]+)?$ ]] || { echo 'Invalid release version' >&2; exit 1; }
archive="plane_${version#v}_${os}_${arch}.tar.gz"
url="https://github.com/$repo/releases/download/$version"
curl -fsSL --retry 3 "$url/$archive" -o "$tmp/$archive"
curl -fsSL --retry 3 "$url/SHA256SUMS" -o "$tmp/SHA256SUMS"
cd "$tmp"
awk -v name="$archive" '$2 == name { print; found=1 } END { if (!found) exit 1 }' SHA256SUMS > selected.sha256
if command -v sha256sum >/dev/null; then sha256sum -c selected.sha256; else shasum -a 256 -c selected.sha256; fi
tar -xzf "$archive" plane
prefix=${PLANE_INSTALL_DIR:-$HOME/.local/bin}
mkdir -p "$prefix"
install -m 755 plane "$prefix/plane"
"$prefix/plane" --version
echo "Installed to $prefix/plane. Ensure $prefix is on PATH."
