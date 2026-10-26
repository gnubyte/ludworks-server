#!/usr/bin/env bash
# Build release binaries for linux/amd64, linux/arm64 and windows/amd64 into dist/.
#
#   scripts/build.sh                 # version from `git describe`, else 0.0.0-dev
#   VERSION=1.2.3 scripts/build.sh
#   TARGETS="linux/amd64" scripts/build.sh
#
# Builds are static (CGO_ENABLED=0), path-independent (-trimpath) and reproducible
# (-buildid=), and use -mod=readonly so a release never silently edits go.mod/go.sum.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

if [[ -z "${VERSION:-}" ]]; then
  VERSION="$(git describe --tags --always --dirty 2>/dev/null || true)"
  VERSION="${VERSION#v}"
  VERSION="${VERSION:-0.0.0-dev}"
fi
# The version is spliced into -ldflags: allow only a conservative character set.
if [[ ! "$VERSION" =~ ^[0-9A-Za-z][0-9A-Za-z.+_-]*$ ]]; then
  echo "invalid VERSION: $VERSION" >&2
  exit 1
fi

TARGETS="${TARGETS:-linux/amd64 linux/arm64 windows/amd64}"
LDFLAGS="-s -w -buildid= -X ludworks/internal/version.Version=${VERSION}"
mkdir -p dist

for t in $TARGETS; do
  os="${t%/*}"; arch="${t#*/}"
  out="dist/ludworks-${os}-${arch}"
  [[ "$os" == "windows" ]] && out="${out}.exe"
  echo "==> ${out} (${VERSION})"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -mod=readonly -trimpath -ldflags "$LDFLAGS" -o "$out" ./cmd/ludworks
done

( cd dist && sha256sum ludworks-* > SHA256SUMS )
echo "==> dist/SHA256SUMS"
cat dist/SHA256SUMS
