#!/usr/bin/env bash
# Fresh-install smoke test of a built release, on this machine's native platform.
#
# It runs the release's own install.sh against the archive in SMOKE_DIST, with the given
# runtime and gateway images, in a throwaway state directory, install directory and Compose
# project, then proves the product works and the workload sandbox is intact:
#   - the installed CLI reports the release version
#   - the Gateway becomes ready (it only is when the Runtime proved its Chromium user-namespace
#     sandbox and seccomp profile, so a host that cannot sandbox fails here, never degrades)
#   - the web UI is served, an Agent lease works and the CDP bridge reaches the real Chromium
#   - no container is privileged, mounts a Docker socket or runs without seccomp, and no
#     Chromium process uses --no-sandbox
# Everything it created (containers, volumes, networks of its project) is removed on exit.
# It never touches another Compose project; the default project is not lensdock-product.
#
# Required environment:
#   SMOKE_DIST            directory with the release assets (archives, SHA256SUMS, install.sh)
#   SMOKE_VERSION         release version, for example 0.3.0
#   SMOKE_RUNTIME_IMAGE   runtime image reference (a digest reference in CI)
#   SMOKE_GATEWAY_IMAGE   gateway image reference
# Optional environment:
#   SMOKE_PULL            always (default) | missing | never; never needs the images locally
#   SMOKE_PORT            Gateway loopback port (default 18581)
#   SMOKE_PROJECT         Compose project name (default lensdock-smoke)
#   SMOKE_LOG_DIR         when set, container logs are written there on exit
set -euo pipefail

die() { printf 'smoke.sh: FAIL: %s\n' "$*" >&2; exit 1; }
step() { printf '\n== %s\n' "$*" >&2; }

: "${SMOKE_DIST:?SMOKE_DIST is required}"
: "${SMOKE_VERSION:?SMOKE_VERSION is required}"
: "${SMOKE_RUNTIME_IMAGE:?SMOKE_RUNTIME_IMAGE is required}"
: "${SMOKE_GATEWAY_IMAGE:?SMOKE_GATEWAY_IMAGE is required}"
pull=${SMOKE_PULL:-always}
port=${SMOKE_PORT:-18581}
project=${SMOKE_PROJECT:-lensdock-smoke}
origin="http://127.0.0.1:$port"

[[ $project != lensdock-product ]] || die "refusing the live project name lensdock-product"
[[ $project =~ ^[a-z0-9][a-z0-9_-]*$ ]] || die "invalid SMOKE_PROJECT: $project"
for cmd in docker curl tar jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing required command: $cmd"
done
docker info >/dev/null 2>&1 || die "the Docker daemon is not reachable"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  aarch64 | arm64) arch=arm64 ;;
  *) die "unsupported CPU: $(uname -m)" ;;
esac
SMOKE_DIST=$(cd "$SMOKE_DIST" && pwd)
archive="$SMOKE_DIST/lensdock_${SMOKE_VERSION}_${os}_${arch}.tar.gz"
[[ -f $archive && -f $SMOKE_DIST/SHA256SUMS && -f $SMOKE_DIST/install.sh ]] || die "$SMOKE_DIST lacks $archive, SHA256SUMS or install.sh"

# Everything labelled with this project is removed on exit, so the project must not exist yet.
project_filter="label=com.docker.compose.project=$project"
existing=$(
  docker ps -aq --filter "$project_filter"
  docker volume ls -q --filter "$project_filter"
  docker network ls -q --filter "$project_filter"
)
[[ -z $existing ]] || die "Compose project $project already has Docker resources; choose another SMOKE_PROJECT"

work=$(mktemp -d)
bridge_pid=""

cleanup() {
  local status=$? id name
  set +e
  [[ -z $bridge_pid ]] || kill "$bridge_pid" 2>/dev/null
  if [[ -n ${SMOKE_LOG_DIR:-} ]]; then
    mkdir -p "$SMOKE_LOG_DIR"
    while read -r id; do
      [[ -n $id ]] || continue
      name=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$id")
      docker logs --tail 500 "$id" >"$SMOKE_LOG_DIR/${name:-$id}.log" 2>&1
    done < <(docker ps -aq --filter "$project_filter")
  fi
  while read -r id; do [[ -z $id ]] || docker rm -f "$id" >/dev/null 2>&1; done < <(docker ps -aq --filter "$project_filter")
  while read -r id; do [[ -z $id ]] || docker volume rm "$id" >/dev/null 2>&1; done < <(docker volume ls -q --filter "$project_filter")
  while read -r id; do [[ -z $id ]] || docker network rm "$id" >/dev/null 2>&1; done < <(docker network ls -q --filter "$project_filter")
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT

step "install $SMOKE_VERSION from $archive into project $project on $origin"
LENSDOCK_INSTALL_ARCHIVE="$archive" \
  LENSDOCK_INSTALL_DIR="$work/lib" LENSDOCK_BIN_DIR="$work/bin" LENSDOCK_STATE_DIR="$work/state" \
  LENSDOCK_COMPOSE_PROJECT="$project" LENSDOCK_PUBLIC_ORIGIN="$origin" \
  LENSDOCK_RUNTIME_IMAGE="$SMOKE_RUNTIME_IMAGE" LENSDOCK_GATEWAY_IMAGE="$SMOKE_GATEWAY_IMAGE" \
  LENSDOCK_PULL="$pull" \
  bash "$SMOKE_DIST/install.sh"

cli="$work/bin/lensdock"
step "the installed CLI reports the release version"
reported=$("$cli" version)
[[ $reported == "lensdock $SMOKE_VERSION ("* ]] || die "'lensdock version' printed '$reported', want 'lensdock $SMOKE_VERSION (<commit>)'"

step "the Gateway becomes ready"
ready=0
for _ in $(seq 1 150); do
  if curl -fsS --max-time 5 "$origin/readyz" >/dev/null 2>&1; then ready=1; break; fi
  sleep 2
done
((ready)) || die "$origin/readyz did not report ready within 300 s"
curl -fsS --max-time 5 "$origin/healthz" >/dev/null || die "$origin/healthz failed"
page=$(curl -fsS --max-time 5 "$origin/") || die "$origin/ failed"
grep -qi '<html' <<<"$page" || die "the Gateway did not serve the web UI at $origin/"

step "an Agent lease reaches the real Chromium through the CDP bridge"
token="$work/state/product/agent-token"
[[ -f $token ]] || die "missing $token"
"$cli" lease acquire default --mode agent --ttl 60 --api "$origin" --operator-token-file "$token" --output "$work/lease.json"
"$cli" cdp bridge --lease-file "$work/lease.json" --listen 127.0.0.1:0 --url-file "$work/bridge.url" >"$work/bridge.log" 2>&1 &
bridge_pid=$!
for _ in $(seq 1 40); do
  [[ -s $work/bridge.url ]] && break
  sleep 0.5
done
[[ -s $work/bridge.url ]] || die "the CDP bridge did not start: $(cat "$work/bridge.log")"
discovery=$(jq -er .discoveryUrl "$work/bridge.url") || die "the CDP bridge did not write a discoveryUrl"
version_json=$(curl -fsS --max-time 10 "$discovery/json/version") || die "CDP /json/version failed"
browser_version=$(jq -er .Browser <<<"$version_json") || die "CDP /json/version has no Browser field"
printf 'CDP: %s\n' "$browser_version" >&2
kill "$bridge_pid" 2>/dev/null || true
wait "$bridge_pid" 2>/dev/null || true
bridge_pid=""
"$cli" lease release --lease-file "$work/lease.json"

step "the workload sandbox is intact"
runtime_id=""
count=0
while read -r id; do
  [[ -n $id ]] || continue
  count=$((count + 1))
  service=$(docker inspect --format '{{index .Config.Labels "com.docker.compose.service"}}' "$id")
  [[ $(docker inspect --format '{{.HostConfig.Privileged}}' "$id") == false ]] || die "$service is privileged"
  mounts=$(docker inspect --format '{{range .Mounts}}{{.Source}} {{end}}' "$id")
  [[ $mounts != *docker.sock* ]] || die "$service mounts a Docker socket"
  opts=$(docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$id")
  [[ $opts == *no-new-privileges* ]] || die "$service lacks no-new-privileges"
  [[ $opts != *unconfined* ]] || die "$service runs with an unconfined security profile"
  [[ $(docker inspect --format '{{json .HostConfig.CapDrop}}' "$id") == *ALL* ]] || die "$service does not drop all capabilities"
  [[ $service != runtime ]] || runtime_id=$id
done < <(docker ps -q --filter "label=com.docker.compose.project=$project")
((count >= 2)) || die "expected the runtime and gateway containers, found $count"
[[ -n $runtime_id ]] || die "no runtime container in project $project"
[[ $(docker inspect --format '{{json .HostConfig.SecurityOpt}}' "$runtime_id") == *seccomp=* ]] || die "the runtime has no custom seccomp profile"
cmdlines=$(docker exec "$runtime_id" /bin/sh -c 'cat /proc/[0-9]*/cmdline 2>/dev/null' | tr '\0' '\n')
[[ $cmdlines == *chromium* ]] || die "no Chromium process found in the runtime container"
[[ $cmdlines != *--no-sandbox* ]] || die "Chromium runs with --no-sandbox"

printf '\nsmoke test passed for %s on %s/%s\n' "$SMOKE_VERSION" "$os" "$arch" >&2
