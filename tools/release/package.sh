#!/usr/bin/env bash
# Build the LensDock GitHub release assets and verify them.
#
# Output (in --out): four archives lensdock_<version>_<darwin|linux>_<amd64|arm64>.tar.gz,
# SHA256SUMS and install.sh. Every archive holds the host CLI (lensdock), both Linux helper
# CLIs (bin/linux-amd64/lensdock, bin/linux-arm64/lensdock), VERSION, docker/compose.yaml,
# docker/browser/seccomp/chromium.json and the READMEs.
#
# Usage: tools/release/package.sh --version X.Y.Z [--out DIR] [--commit SHA] [--installer FILE] [--strict]
#   --version    release version without a leading v (required)
#   --out        output directory, must be empty or absent (default: dist/<version>)
#   --commit     commit stamped into the binaries (default: HEAD)
#   --installer  install.sh to publish (default: install.sh in the repository root)
#   --strict     CI mode: the worktree must be clean and HEAD must equal --commit
#
# Nothing is published and Docker is not needed. The Go toolchain must equal the go.mod
# `toolchain` pin, so run `mise exec -- tools/release/package.sh ...` on a workstation.
set -euo pipefail

die() { printf 'package.sh: %s\n' "$*" >&2; exit 2; }

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
GO=${GO:-go}
version="" out="" commit="" installer="$ROOT/install.sh" strict=0

while (($#)); do
  case $1 in
    --version | --out | --commit | --installer)
      (($# >= 2)) || die "$1 needs a value"
      case $1 in
        --version) version=$2 ;;
        --out) out=$2 ;;
        --commit) commit=$2 ;;
        --installer) installer=$2 ;;
      esac
      shift 2
      ;;
    --strict) strict=1; shift ;;
    *) die "unknown argument: $1 (see the header of this script for usage)" ;;
  esac
done

[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z][0-9A-Za-z.-]*)?$ ]] || die "--version must look like 1.2.3 (no leading v)"
[[ -f $installer ]] || die "installer not found: $installer"
cd "$ROOT"

[[ -n $commit ]] || commit=$(git rev-parse HEAD)
[[ $commit =~ ^[0-9a-f]{40}$ ]] || die "--commit must be a full 40-character lowercase commit hash"
epoch=$(git log -1 --format=%ct "$commit") || die "commit $commit is not in this repository"
if ((strict)); then
  [[ $(git rev-parse HEAD) == "$commit" ]] || die "--strict: HEAD is not $commit"
  [[ -z $(git status --porcelain --untracked-files=normal) ]] || die "--strict: the worktree must be clean"
fi

toolchain=""
while IFS= read -r line || [[ -n $line ]]; do
  if [[ $line =~ ^toolchain[[:space:]]+(go[0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$ ]]; then
    toolchain=${BASH_REMATCH[1]}
    break
  fi
done <go.mod
[[ -n $toolchain ]] || die "go.mod has no 'toolchain goX.Y.Z' line"

export GOFLAGS=-mod=readonly GOTOOLCHAIN=local
[[ $("$GO" env GOVERSION) == "$toolchain" ]] || die "Go toolchain is $("$GO" env GOVERSION), go.mod pins $toolchain (use mise exec --)"

[[ -n $out ]] || out="$ROOT/dist/$version"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

ldflags="-s -w -X main.version=$version -X main.commit=$commit"
for target in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64; do
  goos=${target%/*} goarch=${target#*/}
  printf 'building lensdock %s for %s/%s\n' "$version" "$goos" "$goarch" >&2
  CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch "$GO" build -trimpath -ldflags "$ldflags" \
    -o "$work/build/$goos-$goarch/lensdock" ./cmd/lensdock
done

# trimpath omits -ldflags from Go build metadata. Check the actual native
# executable instead of treating the missing metadata as a missing version.
native="$work/build/$("$GO" env GOHOSTOS)-$("$GO" env GOHOSTARCH)/lensdock"
reported=$("$native" version)
[[ $reported == "lensdock $version ($commit)" ]] || die "native CLI version mismatch: $reported"

# The packer runs on the host whatever GOOS/GOARCH the caller exported.
env -u GOOS -u GOARCH -u CGO_ENABLED "$GO" build -o "$work/bundle" ./tools/release/bundle
"$work/bundle" pack --version "$version" --source-root "$ROOT" --build-dir "$work/build" \
  --out "$out" --installer "$installer" --epoch "$epoch"
"$work/bundle" verify --version "$version" "$out"
printf 'release assets ready: %s\n' "$out" >&2
