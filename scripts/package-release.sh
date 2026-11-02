#!/usr/bin/env bash
set -euo pipefail

# Local build/release tooling only. This script never installs or reloads a host.
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
go_bin=${GO:-go}
version=$(python3 -I -c 'import json,sys; print(json.load(open(sys.argv[1]))["plugins"][0]["version"])' "$root/registry.json")
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo 'Invalid release version' >&2; exit 1; }
output=${1:-"$root/dist"}
mkdir -p -- "$output"
output=$(cd -- "$output" && pwd)
cache=${XDG_CACHE_HOME:-"$HOME/.cache"}
mkdir -p -- "$cache"
scratch=$(mktemp -d "$cache/meta-release.XXXXXX")
trap 'rm -rf -- "$scratch"' EXIT

CGO_ENABLED=1 GOOS=linux GOARCH=amd64 "$go_bin" -C "$root" build -trimpath -buildmode=c-shared -o "$scratch/meta.so" .
nm -D "$scratch/meta.so" > "$scratch/symbols.txt"
grep -q ' T cliproxy_plugin_init$' "$scratch/symbols.txt"
archive="meta_${version}_linux_amd64.zip"
# Exclude generated C headers and any credential/config files from the archive.
zip -q -X -j "$scratch/$archive" "$scratch/meta.so" "$root/LICENSE"
(cd -- "$scratch" && sha256sum "$archive") > "$scratch/checksums.txt"
cp -- "$scratch/$archive" "$scratch/checksums.txt" "$output/"
printf 'Created %s and checksums.txt\n' "$archive"
