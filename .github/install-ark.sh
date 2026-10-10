#!/usr/bin/env bash
# Vendored CI subset of Elk's install-ark.sh: release pin + published checksum.
set -euo pipefail
version="$(tr -d '[:space:]' < "$(dirname "$0")/ark-version")"
archive="ark_${version}_linux_amd64.tar.gz"
url="https://github.com/elk-work/ark/releases/download/${version}/${archive}"
dest="${ARK_BIN_DIR:?set ARK_BIN_DIR}"
mkdir -p "$dest"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
curl -sSfL "$url" -o "$tmp/$archive"
curl -sSfL "${url}.sha256" -o "$tmp/checksum"
want="$(awk '{print $1; exit}' "$tmp/checksum")"
got="$(sha256sum "$tmp/$archive" | awk '{print $1}')"
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo 'install-ark: checksum mismatch; nothing installed' >&2
  exit 1
fi
tar -xzf "$tmp/$archive" -C "$tmp"
if [ -f "$tmp/ark" ]; then
  install "$tmp/ark" "$dest/ark"
else
  install "$tmp/ark_${version}_linux_amd64/ark" "$dest/ark"
fi
