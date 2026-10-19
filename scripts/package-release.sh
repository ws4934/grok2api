#!/usr/bin/env bash
# Package the linux/amd64 release archive consumed by the in-app updater.
# Usage: scripts/package-release.sh <vX.Y.Z> <outdir>
# Requires frontend/dist (pnpm build) to exist.
set -euo pipefail

tag=${1:?usage: package-release.sh <vX.Y.Z> <outdir>}
out=${2:?usage: package-release.sh <vX.Y.Z> <outdir>}
[[ $tag =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "invalid tag '$tag', want vX.Y.Z" >&2; exit 1; }

root=$(cd "$(dirname "$0")/.." && pwd)
[[ -f $root/frontend/dist/index.html ]] || { echo "frontend/dist is missing, run pnpm build first" >&2; exit 1; }

mkdir -p "$out"
out=$(cd "$out" && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT

for arch in amd64; do
  dir=$stage/$arch
  mkdir -p "$dir/frontend"
  (cd "$root/backend" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch go build -trimpath -buildvcs=false \
    -ldflags "-s -w -X github.com/chenyme/grok2api/backend/internal/buildinfo.Version=$tag" \
    -o "$dir/grok2api" ./cmd/grok2api)
  chmod 0755 "$dir/grok2api"
  printf '%s' "$tag" > "$dir/VERSION"
  cp -R "$root/frontend/dist" "$dir/frontend/dist"
  tar -C "$dir" -czf "$out/grok2api_${tag}_linux_${arch}.tar.gz" grok2api VERSION frontend
done

cd "$out"
sha256sum "grok2api_${tag}_linux_amd64.tar.gz" > checksums.txt
