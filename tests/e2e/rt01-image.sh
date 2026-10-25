#!/usr/bin/env bash
# RT-01: the runtime image is the approved Debian slim base with an identifiable
# package list, and no Alpine layer.
#
# Usage: tests/e2e/rt01-image.sh [IMAGE]       IMAGE defaults to lensdock/runtime:dev
# Output: $LENSDOCK_E2E_OUT/rt01/{os-release,dpkg-manifest.tsv,labels.json,result.txt}; exit 1 on failure.
# Runs short-lived `docker run --rm --entrypoint <coreutil>` containers; never starts the runtime.

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

IMAGE=${1:-lensdock/runtime:dev}
D=$OUT/rt01
mkdir -p "$D"
: >"$D/result.txt"
fail=0

# check DESCRIPTION CMD...: record PASS/FAIL by the command's exit status.
check() {
  local desc=$1
  shift
  if "$@"; then echo "PASS $desc" >>"$D/result.txt"; else echo "FAIL $desc" >>"$D/result.txt"; fail=1; fi
}
# exits_1 CMD...: succeeds only when CMD exits with status exactly 1 ("absent" for test/grep).
# Any other status (docker 125-127, grep 2, a signal) is a failure to check, not a pass.
# shellcheck disable=SC2329 # used indirectly through check
exits_1() {
  local rc=0
  "$@" || rc=$?
  [ "$rc" -eq 1 ]
}
in_image() { docker run --rm --network none --entrypoint "$1" "$IMAGE" "${@:2}"; }

in_image /bin/cat /etc/os-release >"$D/os-release"
in_image /bin/cat /usr/share/lensdock/dpkg-manifest.tsv >"$D/dpkg-manifest.tsv"
docker image inspect --format '{{json .Config.Labels}}' "$IMAGE" >"$D/labels.json"

check "os-release ID=debian" grep -q '^ID=debian$' "$D/os-release"
check "os-release VERSION_CODENAME=trixie" grep -q '^VERSION_CODENAME=trixie$' "$D/os-release"
check "no /etc/alpine-release" exits_1 in_image /usr/bin/test -e /etc/alpine-release
check "no apk binary" exits_1 in_image /usr/bin/test -e /sbin/apk

pinned=$(base_digest)
check "base digest label equals the Dockerfile pin ($pinned)" \
  grep -q "\"org.opencontainers.image.base.digest\":\"$pinned\"" "$D/labels.json"
check "base name label is debian:trixie-slim" \
  grep -q '"org.opencontainers.image.base.name":"debian:trixie-slim"' "$D/labels.json"

# Every package of packages.txt appears in the manifest with a version and architecture.
# A missing or empty list must fail, not silently skip the package checks.
PKG_LIST=$ROOT/docker/browser/packages.txt
check "package list $PKG_LIST is present and non-empty" grep -qEv '^[[:space:]]*(#|$)' "$PKG_LIST"
while IFS= read -r pkg; do
  check "package $pkg in the dpkg manifest" grep -q "^$pkg	[^	]\+	[a-z0-9]\+$" "$D/dpkg-manifest.tsv"
done < <(grep -Ev '^[[:space:]]*(#|$)' "$PKG_LIST" 2>/dev/null || true)

check "chromium-sandbox absent (userns-only image)" exits_1 grep -q '^chromium-sandbox	' "$D/dpkg-manifest.tsv"

record_hashes "$D"
cat "$D/result.txt"
exit "$fail"
