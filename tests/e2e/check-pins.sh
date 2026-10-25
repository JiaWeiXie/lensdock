#!/usr/bin/env bash
# Verify that the Go version pin agrees across mise.toml, go.mod and docker/browser/Dockerfile,
# and that every base image in the Dockerfile is pinned to a sha256 digest.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
mise_file="$root/mise.toml"
mod_file="$root/go.mod"
docker_file="$root/docker/browser/Dockerfile"

fail=0
err() {
  echo "check-pins: $*" >&2
  fail=1
}

# first_match FILE ERE -> prints capture group 1 of the first matching line, or nothing.
first_match() {
  local file=$1 pattern=$2 line
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line =~ $pattern ]]; then
      printf '%s\n' "${BASH_REMATCH[1]}"
      return 0
    fi
  done <"$file"
  return 0
}

for f in "$mise_file" "$mod_file" "$docker_file"; do
  [[ -f $f ]] || err "missing file: ${f#"$root"/}"
done
((fail == 0)) || exit 1

mise_go=$(first_match "$mise_file" '^go[[:space:]]*=[[:space:]]*"([0-9]+\.[0-9]+\.[0-9]+)"')
mod_toolchain=$(first_match "$mod_file" '^toolchain[[:space:]]+go([0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$')
mod_go=$(first_match "$mod_file" '^go[[:space:]]+([0-9]+\.[0-9]+(\.[0-9]+)?)[[:space:]]*$')
docker_go=$(first_match "$docker_file" '^ARG[[:space:]]+GO_IMAGE=golang:([0-9]+\.[0-9]+\.[0-9]+)-trixie@sha256:[0-9a-f]{64}[[:space:]]*$')
base_digest=$(first_match "$docker_file" '^ARG[[:space:]]+BASE_DIGEST=(sha256:[0-9a-f]{64})[[:space:]]*$')

[[ -n $mise_go ]] || err "mise.toml: no 'go = \"x.y.z\"' entry"
[[ -n $mod_toolchain ]] || err "go.mod: no 'toolchain goX.Y.Z' line"
[[ -n $mod_go ]] || err "go.mod: no 'go X.Y' line"
[[ -n $docker_go ]] || err "Dockerfile: no 'ARG GO_IMAGE=golang:X.Y.Z-trixie@sha256:<digest>' line"
[[ -n $base_digest ]] || err "Dockerfile: no 'ARG BASE_DIGEST=sha256:<digest>' line"
((fail == 0)) || exit 1

[[ $mise_go == "$mod_toolchain" ]] || err "mise.toml go=$mise_go but go.mod toolchain=go$mod_toolchain"
[[ $mise_go == "$docker_go" ]] || err "mise.toml go=$mise_go but Dockerfile GO_IMAGE=golang:$docker_go-trixie"
[[ $mod_toolchain == "$mod_go" || $mod_toolchain == "$mod_go".* ]] ||
  err "go.mod: toolchain go$mod_toolchain is not within the 'go $mod_go' language version"

# Every FROM must resolve to a digest-pinned image (directly or through a pinned ARG).
while IFS= read -r line || [[ -n $line ]]; do
  if [[ $line =~ ^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?([^[:space:]]+) ]]; then
    ref=${BASH_REMATCH[2]}
    case $ref in
      \$\{GO_IMAGE\} | \$\{DEBIAN_IMAGE\}) ;;
      os-base | gobuild) ;;
      *@sha256:*) ;;
      *) err "Dockerfile: FROM $ref is not digest-pinned" ;;
    esac
  fi
done <"$docker_file"

if grep -Eq 'jlesage' "$docker_file"; then
  err "Dockerfile must not reference jlesage"
fi

((fail == 0)) || exit 1
echo "check-pins: ok (go $mise_go, base $base_digest)"
