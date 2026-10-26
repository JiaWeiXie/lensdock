#!/usr/bin/env bash
# Sandbox matrix. Two cells, each started through docker compose:
#   1 userns-custom-seccomp  userns image + docker/browser/seccomp/chromium.json + no-new-privileges   expect: ready
#   2 userns-default-seccomp same image, Docker default seccomp (compose.default-seccomp.yaml)          expect: SANDBOX_PREFLIGHT_FAILED,
#                            runtime not ready, no Chromium process, never --no-sandbox
# Per cell it collects /readyz?detail=1, the workspace, a lease attempt, the container process table
# (uid, NoNewPrivs, Seccomp, caps, NSpid of every process), docker inspect security options and the
# container log tail, and runs TestRT02 when the runtime is ready. Summary: matrix.json.
#
# Usage: tests/e2e/sandbox-matrix.sh [cell ...]     (cells: 1 2; default all)
# Needs: image lensdock/runtime:dev, .lensdock/runtime.env, docker compose >= 2.24,
#        bin/ host binaries for TestRT02 (mise run build e2e:build-host; Linux host: bin/linux-<arch>).
# Output: $LENSDOCK_E2E_OUT/sandbox-matrix/<cell>/... and $LENSDOCK_E2E_OUT/sandbox-matrix/matrix.json.
# Side effect: restores the default runtime configuration at the end.

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

M=$OUT/sandbox-matrix
mkdir -p "$M"
BASE=$ROOT/docker/browser/compose.yaml
NOSECCOMP=$E2E_DIR/compose.default-seccomp.yaml
SANDBOX=$M/compose.sandbox.yaml
# Both existing cells exercise the opt-in sandbox without weakening their checks.
printf 'services:\n  runtime:\n    environment:\n      LENSDOCK_CHROMIUM_SANDBOX: "true"\n' >"$SANDBOX"
CELLS=("$@")
if [ "${#CELLS[@]}" -eq 0 ]; then CELLS=(1 2); fi

# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() { docker compose -f "$BASE" up -d --wait >/dev/null 2>&1 || log "could not restore the default runtime"; }
trap cleanup EXIT

# run_cell NAME EXPECT COMPOSE_FILE...
run_cell() {
  local name=$1 expect=$2
  shift 2
  local files=() f
  for f in "$@"; do files+=(-f "$f"); done
  files+=(-f "$SANDBOX")
  local d=$M/$name rc=-1
  mkdir -p "$d"
  log "cell $name: restarting runtime ($expect expected)"
  docker compose -f "$BASE" down >/dev/null 2>&1 || true
  # --wait fails when the runtime never turns healthy, which is exactly cell 2's expected outcome.
  docker compose "${files[@]}" up -d --wait --wait-timeout 120 >"$d/compose-up.log" 2>&1 || log "cell $name: compose up --wait returned non-zero (see compose-up.log)"

  # Let the supervisor reach its final state: ready, or locked after the preflight.
  if [ "$expect" = preflight-failed ]; then sleep 20; else wait_ready 90 || log "cell $name: runtime did not become ready"; fi

  api_get /readyz "$d/readyz.json" >"$d/readyz.status"
  readyz_detail "$d/readyz-detail.json" >"$d/readyz-detail.status"
  local ws_status
  ws_status=$(api_get "/api/v1/workspaces/$WORKSPACE" "$d/workspace.json" operator)
  echo "$ws_status" >"$d/workspace.status"
  local acq raw
  # The lease response carries a live capabilityToken: keep only the status and the
  # token-free document (lease metadata, or the error code/message).
  raw=$(mktemp)
  acq=$(acquire_raw AGENT "$raw")
  python3 "$TOOL" strip-token "$raw" >"$d/acquire.json"
  rm -f "$raw"
  echo "$acq" >"$d/acquire.status"
  if [ "$acq" = 201 ]; then
    # Only the 10 s TTL releases this probe lease; its token was discarded with the raw response.
    log "cell $name: probe lease acquired (expires by itself)"
  fi
  proc_snapshot "$d/procs.txt" || log "cell $name: could not snapshot processes"
  docker inspect --format 'Image={{.Config.Image}} SecurityOpt={{json .HostConfig.SecurityOpt}} CapAdd={{json .HostConfig.CapAdd}} CapDrop={{json .HostConfig.CapDrop}}' "$CONTAINER" >"$d/inspect.txt" 2>&1 || true
  docker logs --tail 300 "$CONTAINER" >"$d/container.log" 2>&1 || true

  if [ "$(cat "$d/readyz.status")" = 200 ]; then
    sleep 11 # let the probe lease expire so TestRT02 can take its own
    mkdir -p "$d/rt02"
    run_host_tests '^TestRT02$' "$d/rt02" >"$d/rt02.log" 2>&1 && rc=0 || rc=$?
  fi
  python3 "$TOOL" matrix-cell "$d" "$name" "$expect" "$rc" >"$d/cell.json"
  record_hashes "$d"
  log "cell $name: $(python3 "$TOOL" json-get "$d/cell.json" verdict)"
}

for c in "${CELLS[@]}"; do
  case "$c" in
    1) run_cell userns-custom-seccomp ready "$BASE" ;;
    2) run_cell userns-default-seccomp preflight-failed "$BASE" "$NOSECCOMP" ;;
    *) die "unknown cell $c (use 1 or 2)" ;;
  esac
done

cells=()
for d in "$M"/*/cell.json; do cells+=("$d"); done
python3 "$TOOL" matrix-merge "${cells[@]}" >"$M/matrix.json"
cat "$M/matrix.json"
if python3 "$TOOL" json-get "$M/matrix.json" allAsExpected | grep -qx true; then exit 0; else exit 1; fi
