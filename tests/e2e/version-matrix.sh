#!/usr/bin/env bash
# Record the version matrix every e2e run must be attributed to:
# host OS/kernel/arch, Docker engine and security options, user-namespace sysctls, AppArmor,
# image id/digest/base digest, package versions inside the image, Chromium version, Go and
# agent-browser versions when present, and the exact source state of the test commit.
#
# Usage: tests/e2e/version-matrix.sh [IMAGE]       (default image lensdock/runtime:dev)
# Output: $LENSDOCK_E2E_OUT/version-matrix.txt (default tests/e2e/out/<run-id>/)
# Needs a running runtime container for the chromium and dpkg sections; sections that cannot be
# collected say so explicitly instead of being skipped silently.

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

IMAGE=${1:-lensdock/runtime:dev}
OUTFILE=$OUT/version-matrix.txt
: >"$OUTFILE"

section() { printf '\n## %s\n' "$1" >>"$OUTFILE"; }
# run LABEL CMD...: append the command's output; a failing command is recorded, not fatal.
run() {
  local label=$1
  shift
  printf '$ %s\n' "$label" >>"$OUTFILE"
  if ! "$@" >>"$OUTFILE" 2>&1; then printf '(command failed or unavailable: %s)\n' "$label" >>"$OUTFILE"; fi
}

{
  printf 'run-id: %s\n' "$RUN_ID"
  printf 'date-utc: %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)"
} >>"$OUTFILE"

section host
run 'uname -a' uname -a
if [ -r /etc/os-release ]; then run 'cat /etc/os-release' cat /etc/os-release; else run 'sw_vers' sw_vers; fi
run 'uname -m' uname -m
for key in kernel.unprivileged_userns_clone user.max_user_namespaces kernel.apparmor_restrict_unprivileged_userns \
  kernel.apparmor_restrict_unprivileged_unconfined kernel.unprivileged_userns_apparmor_policy; do
  run "sysctl $key" sysctl "$key"
done
if [ -r /sys/module/apparmor/parameters/enabled ]; then run 'apparmor enabled' cat /sys/module/apparmor/parameters/enabled; fi
if command -v aa-status >/dev/null 2>&1; then run 'aa-status --enabled' aa-status --enabled; fi

section docker
run 'docker version' docker version
run 'docker info (selected)' docker info --format 'engine={{.ServerVersion}} os={{.OperatingSystem}} kernel={{.KernelVersion}} arch={{.Architecture}} cgroup={{.CgroupDriver}}/v{{.CgroupVersion}} security={{json .SecurityOptions}} runtimes={{json .Runtimes}}'
run 'docker compose version' docker compose version
run 'docker buildx version' docker buildx version
section 'userns sysctls as seen by the Docker engine (container view)'
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
  for key in user/max_user_namespaces kernel/unprivileged_userns_clone kernel/apparmor_restrict_unprivileged_userns; do
    run "container /proc/sys/$key" docker exec "$CONTAINER" /bin/cat "/proc/sys/$key"
  done
  run 'container AppArmor profile' docker exec "$CONTAINER" /bin/cat /proc/1/attr/current
  run 'container security options' docker inspect --format 'SecurityOpt={{json .HostConfig.SecurityOpt}} CapAdd={{json .HostConfig.CapAdd}} CapDrop={{json .HostConfig.CapDrop}} ReadonlyRootfs={{.HostConfig.ReadonlyRootfs}}' "$CONTAINER"
else
  echo "(runtime container $CONTAINER not found)" >>"$OUTFILE"
fi

section image
run "image id" docker image inspect --format 'id={{.Id}} arch={{.Architecture}} created={{.Created}}' "$IMAGE"
run "image repo digests" docker image inspect --format '{{json .RepoDigests}}' "$IMAGE"
run "image labels" docker image inspect --format '{{json .Config.Labels}}' "$IMAGE"
printf 'base digest pinned in Dockerfile: %s\n' "$(base_digest)" >>"$OUTFILE"

section 'packages (dpkg manifest inside the container)'
if docker inspect "$CONTAINER" >/dev/null 2>&1; then
  run 'dpkg-manifest chromium/x11vnc/xvfb/openbox/fontconfig/fonts-noto*/xclip' \
    docker exec "$CONTAINER" /bin/grep -E '^(chromium|x11vnc|xvfb|openbox|fontconfig|fonts-noto[a-z-]*|xclip|dbus|tini)	' /usr/share/lensdock/dpkg-manifest.tsv
else
  echo "(runtime container $CONTAINER not found)" >>"$OUTFILE"
fi

section 'chromium (readyz detail)'
detail=$OUT/version-matrix.readyz-detail.json
code=$(readyz_detail "$detail")
if [ "$code" = 200 ] || [ "$code" = 503 ]; then
  printf 'readyz status: %s\nchromiumVersion: %s\nsandbox verified: %s\nfcMatch: %s\n' "$code" \
    "$(python3 "$TOOL" json-get "$detail" detail.chromiumVersion unknown)" \
    "$(python3 "$TOOL" json-get "$detail" detail.sandbox.verified unknown)" \
    "$(python3 "$TOOL" json-get "$detail" detail.fcMatch unknown)" >>"$OUTFILE"
else
  printf '(readyz detail unavailable: HTTP %s)\n' "$code" >>"$OUTFILE"
fi

section tools
if command -v go >/dev/null 2>&1; then run 'go version' go version
elif command -v mise >/dev/null 2>&1; then run 'mise exec -- go version' mise exec -- go version
else echo '(go not present on this host)' >>"$OUTFILE"; fi
if command -v agent-browser >/dev/null 2>&1; then run 'agent-browser --version' agent-browser --version
elif command -v mise >/dev/null 2>&1; then run 'mise exec -- agent-browser --version' mise exec -- agent-browser --version
else echo '(agent-browser not present on this host)' >>"$OUTFILE"; fi
for b in "$HOST_BIN_DIR/lensdock" "$LINUX_BIN_DIR/e2e.test"; do
  if [ -f "$b" ]; then printf 'sha256 %s: %s\n' "$b" "$(sha256_of "$b")" >>"$OUTFILE"; fi
done

section 'test source'
if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
  if [ -z "$(git -C "$ROOT" status --porcelain)" ]; then
    printf 'commit: %s\n' "$(git -C "$ROOT" rev-parse HEAD)" >>"$OUTFILE"
  else
    printf 'commit: uncommitted (base %s)\n' "$(git -C "$ROOT" rev-parse HEAD 2>/dev/null || echo none)" >>"$OUTFILE"
  fi
  # Hash of the content of every tracked and untracked-but-not-ignored file, in sorted order.
  tree=$(
    cd "$ROOT" || exit 1
    git ls-files -z -co --exclude-standard | LC_ALL=C sort -z | while IFS= read -r -d '' f; do
      [ -f "$f" ] && printf '%s  %s\n' "$(sha256_of "$f")" "$f"
    done | sha256_stdin
  )
  printf 'source-tree-sha256 (git ls-files -co --exclude-standard contents): %s\n' "$tree" >>"$OUTFILE"
else
  echo 'commit: unknown (no git checkout on this host); source-tree-sha256: unknown' >>"$OUTFILE"
fi

log "wrote $OUTFILE"
