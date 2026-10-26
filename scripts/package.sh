#!/usr/bin/env bash
# Build the release packages into dist/:
#
#   dist/ludworks_<version>_amd64.deb            Debian/Ubuntu package (linux/amd64)
#   dist/ludworks_<version>_arm64.deb            Debian/Ubuntu package (linux/arm64)
#   dist/ludworks_<version>_windows_amd64.zip    ludworks.exe + README-windows.txt, LICENSE,
#                                                config.example.json, install/uninstall scripts
#   dist/SHA256SUMS
#
# dist/stage/ keeps the unpacked payloads (dist/stage/windows_amd64 is what
# packaging/windows/ludworks.iss turns into the Inno Setup installer).
#
#   scripts/package.sh                         version from the VERSION file, else git describe
#   VERSION=0.2.0-rc.1 scripts/package.sh      explicit semantic version
#   SOURCE_DATE_EPOCH=1767225600 scripts/package.sh   fixed timestamps (default: last commit)
#
# Binaries are static (CGO_ENABLED=0), stripped, -trimpath, -buildvcs=false, with an empty
# build ID; the .deb and .zip use SOURCE_DATE_EPOCH for every timestamp, so a rebuild of the
# same commit is byte-identical. Needs Go, python3 (zip), GNU sed and sha256sum (Linux or WSL).
set -euo pipefail
umask 022

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$root"

die() {
  echo "package.sh: $*" >&2
  exit 1
}
for tool in go python3 sha256sum sed; do
  command -v "$tool" >/dev/null 2>&1 || die "$tool is required"
done

# ---------------------------------------------------------------------------------------
# Version
if [[ -z "${VERSION:-}" ]]; then
  if [[ -f VERSION ]]; then
    VERSION="$(tr -d ' \t\r\n' <VERSION)"
  else
    VERSION="$(git describe --tags --match 'v[0-9]*' --dirty 2>/dev/null || true)"
    VERSION="${VERSION#v}"
    # v0.1.0-3-gabc1234[-dirty] -> 0.1.0+3.gabc1234[.dirty] (semver build metadata).
    if [[ "$VERSION" =~ ^([0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?)-([0-9]+)-(g[0-9a-f]+)(-dirty)?$ ]]; then
      VERSION="${BASH_REMATCH[1]}+${BASH_REMATCH[3]}.${BASH_REMATCH[4]}${BASH_REMATCH[5]:+.dirty}"
    elif [[ "$VERSION" == *-dirty ]]; then
      VERSION="${VERSION%-dirty}+dirty"
    fi
  fi
fi
semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ "$VERSION" =~ $semver ]] || die "VERSION '$VERSION' is not a semantic version (MAJOR.MINOR.PATCH[-PRE][+BUILD])"
# Debian sorts "~" before anything, so a semver pre-release 0.2.0-rc.1 becomes 0.2.0~rc.1.
deb_version="${VERSION//-/\~}"

# ---------------------------------------------------------------------------------------
# Reproducible timestamps
if [[ -z "${SOURCE_DATE_EPOCH:-}" ]]; then
  SOURCE_DATE_EPOCH="$(git log -1 --format=%ct 2>/dev/null || true)"
  if [[ -z "$SOURCE_DATE_EPOCH" ]]; then
    SOURCE_DATE_EPOCH="$(date +%s)"
    echo "package.sh: not a git checkout; SOURCE_DATE_EPOCH defaults to now ($SOURCE_DATE_EPOCH)" >&2
  fi
fi
[[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] || die "SOURCE_DATE_EPOCH must be a Unix timestamp"
export SOURCE_DATE_EPOCH

echo "==> Ludworks ${VERSION} (Debian ${deb_version}), SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH}"

stage=dist/stage
rm -rf "$stage"
rm -f dist/ludworks_*.deb dist/ludworks_*.zip dist/SHA256SUMS
mkdir -p "$stage/tools" "$stage/linux_amd64" "$stage/linux_arm64" "$stage/windows_amd64"

# Host tools (pure Go).
env -u GOOS -u GOARCH CGO_ENABLED=0 go build -mod=readonly -o "$stage/tools/mkdeb" ./tools/mkdeb
env -u GOOS -u GOARCH CGO_ENABLED=0 go build -mod=readonly -o "$stage/tools/winres" ./tools/winres
mkdeb="$stage/tools/mkdeb"
winres="$stage/tools/winres"

ldflags="-s -w -buildid= -X ludworks/internal/version.Version=${VERSION}"
build() { # os arch output
  echo "==> go build $1/$2 -> $3"
  CGO_ENABLED=0 GOOS="$1" GOARCH="$2" go build -mod=readonly -trimpath -buildvcs=false \
    -ldflags "$ldflags" -o "$3" ./cmd/ludworks
}

# ---------------------------------------------------------------------------------------
# Windows: version/icon resources exist only for this build (never committed).
syso=cmd/ludworks/rsrc_windows_amd64.syso
trap 'rm -f "$root/$syso"' EXIT
"$winres" syso -version "$VERSION" -arch amd64 -icon packaging/windows/ludworks.ico -out "$syso"
build windows amd64 "$stage/windows_amd64/ludworks.exe"
rm -f "$syso"
"$winres" verify -version "$VERSION" "$stage/windows_amd64/ludworks.exe"

# Text files get CRLF line endings for Notepad and PowerShell 5.1, whatever the checkout uses.
crlf() { sed -e 's/\r$//' -e 's/$/\r/' "$1" >"$2"; }
crlf packaging/windows/README-windows.txt "$stage/windows_amd64/README-windows.txt"
crlf packaging/windows/config.example.json "$stage/windows_amd64/config.example.json"
crlf packaging/windows/install-service.ps1 "$stage/windows_amd64/install-service.ps1"
crlf packaging/windows/uninstall-service.ps1 "$stage/windows_amd64/uninstall-service.ps1"
crlf LICENSE "$stage/windows_amd64/LICENSE"

zip="dist/ludworks_${VERSION}_windows_amd64.zip"
echo "==> $zip"
# Deterministic zip: fixed order, fixed timestamps, no host permissions.
python3 -I - "$zip" "$SOURCE_DATE_EPOCH" "$stage/windows_amd64" \
  ludworks.exe README-windows.txt LICENSE config.example.json install-service.ps1 uninstall-service.ps1 <<'PY'
import os, sys, time, zipfile
out, epoch, src, names = sys.argv[1], int(sys.argv[2]), sys.argv[3], sys.argv[4:]
stamp = time.gmtime(max(epoch, 315532800))[:6]  # zip cannot store dates before 1980
with zipfile.ZipFile(out + ".tmp", "w") as z:
    for name in names:
        with open(os.path.join(src, name), "rb") as f:
            data = f.read()
        info = zipfile.ZipInfo(name, date_time=stamp)
        info.create_system = 0  # MS-DOS attributes
        info.external_attr = 0x20  # FILE_ATTRIBUTE_ARCHIVE
        z.writestr(info, data, compress_type=zipfile.ZIP_DEFLATED, compresslevel=9)
os.replace(out + ".tmp", out)
PY

# ---------------------------------------------------------------------------------------
# Debian packages
for arch in amd64 arm64; do
  bin="$stage/linux_${arch}/ludworks"
  build linux "$arch" "$bin"
  deb="dist/ludworks_${VERSION}_${arch}.deb"
  "$mkdeb" build -spec packaging/deb/mkdeb.json -version "$deb_version" -arch "$arch" \
    -var "BINARY=$root/$bin" -out "$deb"
  "$mkdeb" inspect "$deb" >/dev/null
done

# ---------------------------------------------------------------------------------------
(
  cd dist
  sha256sum "ludworks_${VERSION}_amd64.deb" "ludworks_${VERSION}_arm64.deb" \
    "ludworks_${VERSION}_windows_amd64.zip" >SHA256SUMS
)
echo "==> dist/SHA256SUMS"
cat dist/SHA256SUMS
