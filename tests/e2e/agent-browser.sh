#!/usr/bin/env bash
# SPIKE-04 agent-browser compatibility (macOS only, where agent-browser is installed by mise).
# Flow: acquire an AGENT lease with the lensdock CLI -> start `lensdock cdp bridge` twice (default
# root mode, and --path-secret) -> drive the remote Chromium with `agent-browser --session lensdock-m0
# --cdp <PORT>` (root mode: tab list, eval document.title) and `--cdp <browserWebSocketUrl>` (path-secret
# mode: snapshot) -> revoke the lease -> the NEXT agent-browser command on
# the same session must fail, and no local browser may be launched by it -> close the session.
#
# Usage: tests/e2e/agent-browser.sh
# Needs: runtime ready, bin/lensdock (mise run build), agent-browser on PATH or via `mise exec`.
#        Never runs `agent-browser install`. On non-macOS hosts it prints "not run" and exits 0
#        (recorded in result.txt) because the SPIKE-04 subject is the macOS install.
# Output: $LENSDOCK_E2E_OUT/agent-browser/{versions.txt,*.out,*.err,*.rc,local-browser-diff.txt,result.txt}

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

D=$OUT/agent-browser
mkdir -p "$D"
RES=$D/result.txt
: >"$RES"
if [ "$(host_os)" != darwin ]; then
  echo "not run: agent-browser SPIKE-04 is macOS only (host is $(host_os))" | tee "$RES"
  exit 0
fi

if command -v agent-browser >/dev/null 2>&1; then AB=(agent-browser)
elif command -v mise >/dev/null 2>&1; then AB=(mise exec -- agent-browser)
else die "agent-browser not found (mise install)"; fi
LENSDOCK=$HOST_BIN_DIR/lensdock
[ -x "$LENSDOCK" ] || die "$LENSDOCK missing (mise run build)"
SESSION=lensdock-m0
ab() { python3 "$TOOL" timeout 60 "${AB[@]}" --session "$SESSION" "$@"; }

TMP=$(mktemp -d)
LEASE=$TMP/lease.json
BRIDGE_PID=''
BRIDGE2_PID=''
# shellcheck disable=SC2329 # invoked by the EXIT trap below
cleanup() {
  if [ -n "$BRIDGE_PID" ]; then kill "$BRIDGE_PID" 2>/dev/null || true; fi
  if [ -n "$BRIDGE2_PID" ]; then kill "$BRIDGE2_PID" 2>/dev/null || true; fi
  "$LENSDOCK" lease release --lease-file "$LEASE" --api "$API_URL" >/dev/null 2>&1 || true
  ab close >/dev/null 2>&1 || true
  rm -rf "$TMP"
}
trap cleanup EXIT

record() { echo "$1" | tee -a "$RES"; }
# Only browser main processes count: a newly launched local browser always has
# one (no --type=), while helpers of browsers already running before the test
# (renderer/GPU/utility, all --type=...) come and go on their own.
# shellcheck disable=SC2009 # pgrep cannot print full command lines portably on macOS
local_browsers() { ps -axo command= | grep -Ei 'chrome|chromium|headless' | grep -Ev 'grep|OrbStack|Docker|--type=' | LC_ALL=C sort -u || true; }

{
  "${AB[@]}" --version
  echo "lensdock bin sha256: $(sha256_of "$LENSDOCK")"
  echo "runtime chromium: $(readyz_detail "$TMP/d.json" >/dev/null; python3 "$TOOL" json-get "$TMP/d.json" detail.chromiumVersion unknown)"
} >"$D/versions.txt" 2>&1

wait_ready 90 || die "runtime is not ready at $API_URL"
local_browsers >"$TMP/browsers.before"

"$LENSDOCK" lease acquire "$WORKSPACE" --mode agent --ttl 60 --api "$API_URL" --operator-token-file "$TOKEN_FILE" --output "$LEASE"
# start_bridge OUT ERR [ARG...]: run the bridge in the background (pid in BRIDGE_STARTED), wait for its endpoint line.
start_bridge() {
  local out=$1 err=$2
  shift 2
  "$LENSDOCK" cdp bridge --lease-file "$LEASE" --listen 127.0.0.1:0 "$@" >"$out" 2>"$err" &
  BRIDGE_STARTED=$!
  for _ in $(seq 1 100); do [ -s "$out" ] && break; sleep 0.2; done
  [ -s "$out" ] || die "bridge printed no endpoints (see $err)"
}
# Default mode: no URL prefix, so `--cdp PORT` works directly.
start_bridge "$TMP/bridge.out" "$D/bridge.err"
BRIDGE_PID=$BRIDGE_STARTED
python3 "$TOOL" json-get "$TMP/bridge.out" discoveryUrl >"$D/discovery-url.txt"
DISCOVERY=$(cat "$D/discovery-url.txt")
DISCOVERY=${DISCOVERY%/}
PORT=${DISCOVERY##*:}
case $PORT in '' | *[!0-9]*) die "default-mode discoveryUrl $DISCOVERY is not a bare http://127.0.0.1:PORT" ;; esac
[ "$DISCOVERY" = "http://127.0.0.1:$PORT" ] || die "default-mode discoveryUrl $DISCOVERY carries a path"
# --path-secret mode: the full browserWebSocketUrl keeps its random prefix (SPIKE-04 claim).
start_bridge "$TMP/bridge-secret.out" "$D/bridge-secret.err" --path-secret
BRIDGE2_PID=$BRIDGE_STARTED
WS_SECRET=$(python3 "$TOOL" json-get "$TMP/bridge-secret.out" browserWebSocketUrl)

fails=0
step() { # step NAME CMD...: record the outcome, keep going
  local name=$1 rc=0
  shift
  "$@" >"$D/$name.out" 2>"$D/$name.err" || rc=$?
  echo "$rc" >"$D/$name.rc"
  if [ "$rc" -eq 0 ]; then record "PASS $name"; else record "FAIL $name (exit $rc)"; fails=$((fails + 1)); fi
}
# Default (root) mode through the port form, then the full path-secret WebSocket URL, then the port form again.
step tab-list ab --cdp "$PORT" tab list
step snapshot ab --cdp "$WS_SECRET" snapshot
step eval-title ab --cdp "$PORT" eval document.title
record "document.title: $(tr -d '\n' <"$D/eval-title.out" | cut -c1-120)"

# Revoke from the operator side, wait for the bridge to notice and exit.
"$LENSDOCK" lease release --lease-file "$LEASE" --api "$API_URL" >"$D/release.out" 2>"$D/release.err" || record "note: lease release exited $?"
# wait_bridge PID: wait up to 20 s for the bridge to exit; sets WAIT_RC to its exit code, or "running".
# Not run in a subshell: `wait` only sees children of this shell.
wait_bridge() {
  local pid=$1 rc=0
  for _ in $(seq 1 100); do kill -0 "$pid" 2>/dev/null || break; sleep 0.2; done
  if kill -0 "$pid" 2>/dev/null; then WAIT_RC=running; return 0; fi
  wait "$pid" || rc=$?
  WAIT_RC=$rc
}
wait_bridge "$BRIDGE_PID"
rc_root=$WAIT_RC
if [ "$rc_root" != running ]; then BRIDGE_PID=''; fi
wait_bridge "$BRIDGE2_PID"
rc_secret=$WAIT_RC
if [ "$rc_secret" != running ]; then BRIDGE2_PID=''; fi
if [ "$rc_root" = 3 ] && [ "$rc_secret" = 3 ]; then
  record "PASS both bridges exited 3 after revoke"
else
  record "FAIL bridge exit codes after revoke: root mode $rc_root, --path-secret mode $rc_secret (want 3; \"running\" = still up 20 s after revoke)"
  fails=$((fails + 1))
fi

# The very next command on the session must fail and must not fall back to a local browser.
next_rc=0
ab eval document.title >"$D/after-revoke.out" 2>"$D/after-revoke.err" || next_rc=$?
echo "$next_rc" >"$D/after-revoke.rc"
if [ "$next_rc" -ne 0 ]; then record "PASS command after revoke failed (exit $next_rc)"; else record "FAIL command after revoke succeeded"; fails=$((fails + 1)); fi
local_browsers >"$TMP/browsers.after"
comm -13 "$TMP/browsers.before" "$TMP/browsers.after" >"$D/local-browser-diff.txt" || true
if [ -s "$D/local-browser-diff.txt" ]; then record "FAIL new local browser process(es) appeared, see local-browser-diff.txt"; fails=$((fails + 1)); else record "PASS no local browser was launched"; fi

step close ab close
record_hashes "$D"
exit "$((fails > 0))"
