#!/usr/bin/env bash
set -euo pipefail

# Local build/release tooling only. This script never installs or reloads a host.
root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
# Reject enclosing repositories before creating output or invoking build tools.
# Resolve physical paths so a symlinked checkout is still the same worktree.
git_root=$(git -C "$root" rev-parse --show-toplevel)
git_root=$(cd -- "$git_root" && pwd -P)
[[ "$git_root" == "$root" ]] || {
  echo 'Source root is not the Git worktree root' >&2
  exit 1
}
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

# Bind this worktree explicitly; automatic Go VCS discovery may select an
# enclosing checkout instead. Harness configuration is not a build input.
revision=$(git -C "$root" rev-parse --verify HEAD)
tree=$(git -C "$root" rev-parse 'HEAD^{tree}')
[[ "$revision" =~ ^[0-9a-f]{40}$ && "$tree" =~ ^[0-9a-f]{40}$ ]] || exit 1
source_status=$(git -C "$root" status --porcelain --untracked-files=all -- . ':!.claude') || {
  echo 'Cannot inspect source cleanliness before build' >&2
  exit 1
}
modified=true
if [[ -z "$source_status" ]]; then
  modified=false
fi
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 "$go_bin" -C "$root" build -buildvcs=false -trimpath \
  -ldflags "-X main.replayBuildRevision=$revision -X main.replayBuildModified=$modified" \
  -buildmode=c-shared -o "$scratch/meta.so" .
[[ "$(git -C "$root" rev-parse HEAD)" == "$revision" ]] || { echo 'Source revision changed during build' >&2; exit 1; }
source_status=$(git -C "$root" status --porcelain --untracked-files=all -- . ':!.claude') || {
  echo 'Cannot inspect source cleanliness after build' >&2
  exit 1
}
if [[ "$modified" == false && -n "$source_status" ]]; then
  echo 'Clean source changed during build' >&2
  exit 1
fi
nm -D "$scratch/meta.so" > "$scratch/symbols.txt"
grep -q ' T cliproxy_plugin_init$' "$scratch/symbols.txt"
archive="meta_${version}_linux_amd64.zip"
# Exclude generated C headers and any credential/config files from the archive.
zip -q -X -j "$scratch/$archive" "$scratch/meta.so" "$root/LICENSE"
(cd -- "$scratch" && sha256sum "$archive") > "$scratch/checksums.txt"
python3 -I - "$scratch" "$archive" "$revision" "$tree" "$modified" "$version" "$root/docs/replay-event-v1.schema.json" <<'PY'
import hashlib, json, pathlib, sys
scratch, archive, revision, tree, modified, version, schema = sys.argv[1:]
root = pathlib.Path(scratch)
def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()
manifest = {
    "schema": "meta-build-provenance-v1", "producer_version": version,
    "source_revision": revision, "source_tree": tree, "source_modified": modified == "true",
    "archive": archive, "archive_sha256": digest(root / archive),
    "library_sha256": digest(root / "meta.so"), "schema_sha256": digest(pathlib.Path(schema)),
    "build_attestation": False, "production_acceptance": False,
}
(root / "build-provenance.json").write_text(json.dumps(manifest, indent=2) + "\n")
PY
cp -- "$scratch/$archive" "$scratch/checksums.txt" "$scratch/build-provenance.json" "$output/"
printf 'Created %s, checksums.txt and build-provenance.json\n' "$archive"
