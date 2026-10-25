#!/usr/bin/env bash
# VNC CPU measurement. Scenarios, DURATION seconds each (default 60):
#   S0 idle           static page, no viewer
#   S1 one viewer     static page, one RFB viewer on 5901
#   S2 animation      full-viewport animation fixture, one viewer on 5900 and one on 5901
#   S3 three viewers  animation fixture, three viewers on 5901
# Per-process CPU comes from the cpuTicks of /readyz?detail=1 units sampled at both ends of the
# measurement window (percent of one vCPU); container-wide CPU from cgroup v2 cpu.stat when present.
#
# Pre-registered rule (decided before measuring): on amd64, if the 5901 marginal CPU in S2
# (vnc-view in S2 minus S0) exceeds 10% of one vCPU, recommend one x11vnc with -viewpasswd instead of
# two instances. On other architectures the number is recorded but non-decisive.
#
# Usage: tests/e2e/vnc-cpu.sh [DURATION_SECONDS]
# Needs: runtime ready and idle (no lease), bin/linux-<arch>/e2e.test; load clients run in the
#        sidecar container (see run-sidecar.sh for the test-only network-namespace pattern).
# Output: $LENSDOCK_E2E_OUT/vnc-cpu/S{0..3}.{before,after}.json, S*.cpu.json, summary.json, verdict.txt.

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

DURATION=${1:-60}
HZ=30
V=$OUT/vnc-cpu
mkdir -p "$V"
wait_ready 90 || die "runtime is not ready at $API_URL"
CLK=$(docker exec "$CONTAINER" getconf CLK_TCK)
now() { python3 -c 'import time; print(time.time())'; }
cgroup_usec() { docker exec "$CONTAINER" /bin/sh -c 'sed -n "s/^usage_usec //p" /sys/fs/cgroup/cpu.stat' 2>/dev/null || true; }

# scenario NAME PAGE PORTS(comma separated, may be empty)
scenario() {
  local name=$1 page=$2 ports=$3 bg='' t0 t1 cg0 cg1 loadrc=0
  log "$name: page=$page viewers=[$ports] for ${DURATION}s"
  sidecar_run '^TestSpike02Page$' "LENSDOCK_E2E_PAGE=$page" "LENSDOCK_E2E_LABEL=$name" >"$V/$name.page.log" 2>&1 \
    || die "$name: could not prepare the page (see $V/$name.page.log)"
  if [ -n "$ports" ]; then
    sidecar_run '^TestSpike02Load$' "LENSDOCK_E2E_LOAD_PORTS=$ports" "LENSDOCK_E2E_LOAD_SECONDS=$((DURATION + 12))" \
      "LENSDOCK_E2E_LOAD_HZ=$HZ" "LENSDOCK_E2E_LABEL=$name" >"$V/$name.load.log" 2>&1 &
    bg=$!
  fi
  sleep 6 # viewers connected, first full frames delivered
  [ "$(readyz_detail "$V/$name.before.json")" = 200 ] || die "$name: readyz detail failed before the window"
  cg0=$(cgroup_usec)
  t0=$(now)
  sleep "$DURATION"
  [ "$(readyz_detail "$V/$name.after.json")" = 200 ] || die "$name: readyz detail failed after the window"
  t1=$(now)
  cg1=$(cgroup_usec)
  if [ -n "$bg" ]; then wait "$bg" || loadrc=$?; fi
  if [ "$loadrc" -ne 0 ]; then log "$name: load client exited $loadrc (see $V/$name.load.log)"; fi
  if [ -n "$cg0" ] && [ -n "$cg1" ]; then
    python3 "$TOOL" cpu-delta "$V/$name.before.json" "$V/$name.after.json" "$t0" "$t1" "$CLK" "$cg0" "$cg1" >"$V/$name.cpu.json"
  else
    python3 "$TOOL" cpu-delta "$V/$name.before.json" "$V/$name.after.json" "$t0" "$t1" "$CLK" >"$V/$name.cpu.json"
  fi
  log "$name: vnc-view $(python3 "$TOOL" json-get "$V/$name.cpu.json" units.vnc-view.percentOfOneVcpu n/a)% vnc-control $(python3 "$TOOL" json-get "$V/$name.cpu.json" units.vnc-control.percentOfOneVcpu n/a)% of one vCPU"
  return "$loadrc"
}

rc=0
scenario S0 static "" || rc=1
scenario S1 static 5901 || rc=1
scenario S2 animation 5900,5901 || rc=1
scenario S3 animation 5901,5901,5901 || rc=1
sidecar_run '^TestSpike02Page$' LENSDOCK_E2E_PAGE=static LENSDOCK_E2E_LABEL=restore >/dev/null 2>&1 || log "could not restore the static page"

python3 "$TOOL" vnc-summary "$ARCH" "$V/S0.cpu.json" "$V/S1.cpu.json" "$V/S2.cpu.json" "$V/S3.cpu.json" >"$V/summary.json"
python3 "$TOOL" json-get "$V/summary.json" verdict >"$V/verdict.txt"
cat "$V/summary.json"
record_hashes "$V"
exit "$rc"
