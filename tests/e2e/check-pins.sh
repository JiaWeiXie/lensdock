#!/usr/bin/env bash
# Verify that the Go version pin agrees across mise.toml, go.mod and docker/browser/Dockerfile,
# that both final images share one digest-pinned Debian base, and that they enforce the
# minimum apt version required by the image vulnerability gate.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
mise_file="$root/mise.toml"
mod_file="$root/go.mod"
runtime_docker_file="$root/docker/browser/Dockerfile"
gateway_docker_file="$root/docker/gateway/Dockerfile"

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

for f in "$mise_file" "$mod_file" "$runtime_docker_file" "$gateway_docker_file"; do
  [[ -f $f ]] || err "missing file: ${f#"$root"/}"
done
((fail == 0)) || exit 1

mise_go=$(first_match "$mise_file" '^go[[:space:]]*=[[:space:]]*"([0-9]+\.[0-9]+\.[0-9]+)"')
mod_toolchain=$(first_match "$mod_file" '^toolchain[[:space:]]+go([0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$')
mod_go=$(first_match "$mod_file" '^go[[:space:]]+([0-9]+\.[0-9]+(\.[0-9]+)?)[[:space:]]*$')
docker_go=$(first_match "$runtime_docker_file" '^ARG[[:space:]]+GO_IMAGE=golang:([0-9]+\.[0-9]+\.[0-9]+)-trixie@sha256:[0-9a-f]{64}[[:space:]]*$')
runtime_base_digest=$(first_match "$runtime_docker_file" '^ARG[[:space:]]+BASE_DIGEST=(sha256:[0-9a-f]{64})[[:space:]]*$')
gateway_base_digest=$(first_match "$gateway_docker_file" '^ARG[[:space:]]+BASE_DIGEST=(sha256:[0-9a-f]{64})[[:space:]]*$')
runtime_apt_min=$(first_match "$runtime_docker_file" '^ARG[[:space:]]+APT_MIN_VERSION=([0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$')
gateway_apt_min=$(first_match "$gateway_docker_file" '^ARG[[:space:]]+APT_MIN_VERSION=([0-9]+\.[0-9]+\.[0-9]+)[[:space:]]*$')

[[ -n $mise_go ]] || err "mise.toml: no 'go = \"x.y.z\"' entry"
[[ -n $mod_toolchain ]] || err "go.mod: no 'toolchain goX.Y.Z' line"
[[ -n $mod_go ]] || err "go.mod: no 'go X.Y' line"
[[ -n $docker_go ]] || err "Dockerfile: no 'ARG GO_IMAGE=golang:X.Y.Z-trixie@sha256:<digest>' line"
[[ -n $runtime_base_digest ]] || err "runtime Dockerfile: no 'ARG BASE_DIGEST=sha256:<digest>' line"
[[ -n $gateway_base_digest ]] || err "gateway Dockerfile: no 'ARG BASE_DIGEST=sha256:<digest>' line"
[[ -n $runtime_apt_min ]] || err "runtime Dockerfile: no 'ARG APT_MIN_VERSION=x.y.z' line"
[[ -n $gateway_apt_min ]] || err "gateway Dockerfile: no 'ARG APT_MIN_VERSION=x.y.z' line"
((fail == 0)) || exit 1

[[ $mise_go == "$mod_toolchain" ]] || err "mise.toml go=$mise_go but go.mod toolchain=go$mod_toolchain"
[[ $mise_go == "$docker_go" ]] || err "mise.toml go=$mise_go but Dockerfile GO_IMAGE=golang:$docker_go-trixie"
[[ $mod_toolchain == "$mod_go" || $mod_toolchain == "$mod_go".* ]] ||
  err "go.mod: toolchain go$mod_toolchain is not within the 'go $mod_go' language version"
[[ $runtime_base_digest == "$gateway_base_digest" ]] ||
  err "runtime base $runtime_base_digest does not match gateway base $gateway_base_digest"
[[ $runtime_apt_min == "$gateway_apt_min" ]] ||
  err "runtime apt floor $runtime_apt_min does not match gateway floor $gateway_apt_min"
[[ $runtime_apt_min == 3.0.4 ]] || err "apt floor must be 3.0.4 (got $runtime_apt_min)"
grep -Fq "org.opencontainers.image.base.digest=\"\${BASE_DIGEST}\"" "$gateway_docker_file" ||
  err "gateway Dockerfile: base digest label must use \${BASE_DIGEST}"
for f in "$runtime_docker_file" "$gateway_docker_file"; do
  grep -Eq 'dpkg --compare-versions .*APT_MIN_VERSION' "$f" ||
    err "${f#"$root"/}: apt version floor is not enforced"
done

# Every FROM must resolve to a digest-pinned image (directly or through a pinned ARG).
for f in "$runtime_docker_file" "$gateway_docker_file"; do
  while IFS= read -r line || [[ -n $line ]]; do
    if [[ $line =~ ^FROM[[:space:]]+(--platform=[^[:space:]]+[[:space:]]+)?([^[:space:]]+) ]]; then
      ref=${BASH_REMATCH[2]}
      case $ref in
        \$\{GO_IMAGE\} | \$\{NODE_IMAGE\} | \$\{DEBIAN_IMAGE\}) ;;
        os-base | gobuild | webbuild) ;;
        *@sha256:*) ;;
        *) err "${f#"$root"/}: FROM $ref is not digest-pinned" ;;
      esac
    fi
  done <"$f"

  if grep -Eq 'jlesage' "$f"; then
    err "${f#"$root"/}: must not reference jlesage"
  fi
done

((fail == 0)) || exit 1
echo "check-pins: ok (go $mise_go, base $runtime_base_digest, apt >= $runtime_apt_min)"
