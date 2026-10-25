#!/usr/bin/env bash
# Shared helpers for the LensDock M0 e2e scripts. Source it, do not execute it:
#
#   # shellcheck source=tests/e2e/lib.sh
#   source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"
#
# Depends only on bash, coreutils, docker, curl and python3 (stdlib) plus the copied
# binaries (bin/<os>/lensdock, e2e.test); never on mise, go or jq, so the same scripts
# run on the macOS+OrbStack dev host and on a bare Ubuntu host that has only docker.
#
# Environment (all optional):
#   LENSDOCK_E2E_RUN_ID      run id, default UTC timestamp
#   LENSDOCK_E2E_OUT         evidence dir, default tests/e2e/out/<run-id>
#   LENSDOCK_E2E_CONTAINER   runtime container name, default lensdock-runtime
#   LENSDOCK_E2E_API         API base URL on the host, default http://127.0.0.1:18080
#   LENSDOCK_STATE_DIR       state dir holding operator-token, default <repo>/.lensdock
#   LENSDOCK_E2E_SIDECAR_IMAGE  image for sidecar tests, default debian:trixie-slim@<Dockerfile BASE_DIGEST>
#   LENSDOCK_E2E_TOOL_TIMEOUT   seconds per sidecar/host test run, default 1200

set -euo pipefail

E2E_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$E2E_DIR/../.." && pwd)
export ROOT E2E_DIR

RUN_ID=${LENSDOCK_E2E_RUN_ID:-$(date -u +%Y%m%dT%H%M%SZ)}
OUT=${LENSDOCK_E2E_OUT:-$ROOT/tests/e2e/out/$RUN_ID}
CONTAINER=${LENSDOCK_E2E_CONTAINER:-lensdock-runtime}
API_URL=${LENSDOCK_E2E_API:-http://127.0.0.1:18080}
STATE_DIR=${LENSDOCK_STATE_DIR:-$ROOT/.lensdock}
TOKEN_FILE=$STATE_DIR/operator-token
COMPOSE_FILE=${LENSDOCK_E2E_COMPOSE:-$ROOT/docker/browser/compose.yaml}
TOOL=$E2E_DIR/e2e_tool.py
PROCSNAP=$E2E_DIR/internal/harness/procsnap.sh
WORKSPACE=${LENSDOCK_E2E_WORKSPACE:-default}
TEST_TIMEOUT=${LENSDOCK_E2E_TOOL_TIMEOUT:-1200}
export RUN_ID OUT CONTAINER API_URL STATE_DIR TOKEN_FILE TOOL

mkdir -p "$OUT"

log() { printf '%s [%s] %s\n' "$(date -u +%H:%M:%S)" "$(basename "$0")" "$*" >&2; }
die() { log "ERROR: $*"; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"; }
need docker
need curl
need python3

host_os() { uname -s | tr '[:upper:]' '[:lower:]'; }

# docker_arch prints amd64 or arm64 for the architecture the Docker engine runs containers on.
docker_arch() {
  case "$(docker info --format '{{.Architecture}}')" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) die "unsupported docker architecture: $(docker info --format '{{.Architecture}}')" ;;
  esac
}

ARCH=$(docker_arch)
LINUX_BIN_DIR=${LENSDOCK_E2E_LINUX_BIN_DIR:-$ROOT/bin/linux-$ARCH}
# Host-native binaries: bin/ on macOS (mise run build, e2e:build-host); bin/linux-<arch> on Linux.
if [ "$(host_os)" = linux ]; then HOST_BIN_DIR=$LINUX_BIN_DIR; else HOST_BIN_DIR=${LENSDOCK_E2E_HOST_BIN_DIR:-$ROOT/bin}; fi
export ARCH LINUX_BIN_DIR HOST_BIN_DIR

# base_digest prints the pinned debian base digest from the Dockerfile.
base_digest() {
  sed -n 's/^ARG BASE_DIGEST=\(sha256:[0-9a-f]*\)$/\1/p' "$ROOT/docker/browser/Dockerfile" | head -n 1
}
SIDECAR_IMAGE=${LENSDOCK_E2E_SIDECAR_IMAGE:-debian:trixie-slim@$(base_digest)}

sha256_stdin() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1; else shasum -a 256 | cut -d' ' -f1; fi
}
sha256_of() { sha256_stdin <"$1"; }

# record_hashes writes sha256sum-style lines for every file under dir (except the index itself).
record_hashes() {
  local dir=$1 f
  : >"$dir/files.sha256"
  # shellcheck disable=SC2044
  for f in $(find "$dir" -type f ! -name files.sha256 | LC_ALL=C sort); do
    printf '%s  %s\n' "$(sha256_of "$f")" "${f#"$dir"/}" >>"$dir/files.sha256"
  done
}

# auth_header_file creates a 0600 temp file with the operator Authorization header so the token
# never appears in a process argument list (curl -H @file).
auth_header_file() {
  [ -r "$TOKEN_FILE" ] || die "operator token file not readable: $TOKEN_FILE (run lensdock admin init)"
  local f
  f=$(mktemp)
  chmod 600 "$f"
  printf 'Authorization: Bearer %s\n' "$(tr -d '[:space:]' <"$TOKEN_FILE")" >"$f"
  echo "$f"
}

# api_get PATH OUTFILE [operator]: prints the HTTP status (000 when unreachable).
api_get() {
  local path=$1 out=$2 mode=${3:-} hdr='' code
  if [ "$mode" = operator ]; then hdr=$(auth_header_file); fi
  if [ -n "$hdr" ]; then
    code=$(curl -sS --max-time 15 -o "$out" -w '%{http_code}' -H "@$hdr" "$API_URL$path" 2>/dev/null) || code=000
    rm -f "$hdr"
  else
    code=$(curl -sS --max-time 15 -o "$out" -w '%{http_code}' "$API_URL$path" 2>/dev/null) || code=000
  fi
  echo "$code"
}

readyz_detail() { api_get '/readyz?detail=1' "$1" operator; }

# acquire_raw MODE OUTFILE: POST a lease with the operator token; prints the HTTP status.
acquire_raw() {
  local mode=$1 out=$2 hdr code
  hdr=$(auth_header_file)
  code=$(curl -sS --max-time 15 -o "$out" -w '%{http_code}' -X POST -H "@$hdr" \
    -H 'Content-Type: application/json' -H "Idempotency-Key: e2e-script-$RANDOM-$RANDOM" \
    --data "{\"mode\":\"$mode\",\"ttlSeconds\":10}" \
    "$API_URL/api/v1/workspaces/$WORKSPACE/leases" 2>/dev/null) || code=000
  rm -f "$hdr"
  echo "$code"
}

# wait_ready SECONDS: poll /readyz until 200.
wait_ready() {
  local deadline=$((SECONDS + $1)) tmp
  tmp=$(mktemp)
  while [ "$SECONDS" -lt "$deadline" ]; do
    if [ "$(api_get /readyz "$tmp")" = 200 ]; then rm -f "$tmp"; return 0; fi
    sleep 2
  done
  rm -f "$tmp"
  return 1
}

# proc_snapshot OUTFILE: process table of the runtime container (constant script on stdin).
proc_snapshot() {
  docker exec -i "$CONTAINER" /bin/bash -s <"$PROCSNAP" >"$1"
}

# host_test_env prints the environment of the host-side e2e binary, one NAME=VALUE per line.
host_test_env() {
  printf '%s\n' \
    "LENSDOCK_E2E_API=$API_URL" \
    "LENSDOCK_E2E_OPERATOR_TOKEN_FILE=$TOKEN_FILE" \
    "LENSDOCK_E2E_WORKSPACE=$WORKSPACE" \
    "LENSDOCK_E2E_CONTAINER=$CONTAINER" \
    "LENSDOCK_E2E_LENSDOCK_BIN=$HOST_BIN_DIR/lensdock"
}

# run_host_tests REGEX [OUTDIR]: run tests that talk to the published API and use docker exec.
run_host_tests() {
  local regex=$1 outdir=${2:-$OUT} bin=$HOST_BIN_DIR/e2e.test
  [ -x "$bin" ] || die "$bin missing (mise run e2e:build-host, or copy the Linux binary)"
  [ -x "$HOST_BIN_DIR/lensdock" ] || die "$HOST_BIN_DIR/lensdock missing (mise run build)"
  mkdir -p "$outdir"
  local envs=() line
  while IFS= read -r line; do envs+=("$line"); done < <(host_test_env)
  env "${envs[@]}" "LENSDOCK_E2E_OUT=$outdir" \
    "$bin" -test.run "$regex" -test.v -test.count=1 -test.timeout "${TEST_TIMEOUT}s"
}

# sidecar_run REGEX [ENV=VALUE ...] runs e2e.test in a throwaway
# container that shares the runtime container's network namespace.
#
# TEST-ONLY PATTERN: this is how the suite reaches the loopback-only VNC (5900/5901) and CDP (9223)
# ports and serves tests/fixtures on 127.0.0.1:18081 without publishing a port or mounting anything
# into the runtime. Never copy it into the product.
sidecar_run() {
  local regex=$1
  shift
  [ -x "$LINUX_BIN_DIR/e2e.test" ] || die "$LINUX_BIN_DIR/e2e.test missing (mise run build:linux)"
  local extra=() kv
  for kv in "$@"; do extra+=(-e "$kv"); done
  # Run as the invoking user so evidence written to $OUT stays deletable without root
  # (and the 0600 operator token stays readable only by that same UID).
  docker run --rm --network "container:$CONTAINER" --user "$(id -u):$(id -g)" -e HOME=/tmp \
    -v "$LINUX_BIN_DIR:/e2e:ro" \
    -v "$ROOT/tests/fixtures:/srv/fixtures:ro" \
    -v "$OUT:/out" \
    -v "$TOKEN_FILE:/run/secrets/operator-token:ro" \
    -e LENSDOCK_E2E_API=http://127.0.0.1:8080 \
    -e LENSDOCK_E2E_OPERATOR_TOKEN_FILE=/run/secrets/operator-token \
    -e LENSDOCK_E2E_WORKSPACE="$WORKSPACE" \
    -e LENSDOCK_E2E_OUT=/out \
    -e LENSDOCK_E2E_FIXTURES=/srv/fixtures \
    "${extra[@]+"${extra[@]}"}" \
    "$SIDECAR_IMAGE" /e2e/e2e.test -test.run "$regex" -test.v -test.count=1 -test.timeout "${TEST_TIMEOUT}s"
}

compose() { docker compose -f "$COMPOSE_FILE" "$@"; }
