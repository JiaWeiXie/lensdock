#!/usr/bin/env bash
# Run the sidecar e2e tests: TestCT04 (view-only port ignores forged input), TestTX01 (CJK fonts,
# browser + VNC captures), TestTX02 (emoji shaping), TestTX03 (HUMAN clipboard -> Ctrl+V byte-exact),
# TestSpike03CutText (ClientCutText behaviour) and TestRT03Viewers (viewers disconnecting).
#
# TEST-ONLY PATTERN: e2e.test runs in a throwaway container started with
#   docker run --rm --network container:lensdock-runtime ...
# so 127.0.0.1 is the runtime container's loopback, which is the only place the VNC ports 5900/5901
# and CDP port 9223 listen. tests/fixtures is mounted into the sidecar (not the runtime) and served on
# 127.0.0.1:18081 inside the shared network namespace. Nothing in the product image or compose file
# is changed or published for this; never reproduce it in the product.
#
# Usage: tests/e2e/run-sidecar.sh [TEST_REGEX]
#   default '^(TestCT04|TestTX01|TestTX02|TestTX03|TestSpike03CutText|TestRT03Viewers)$'
# Needs: runtime ready, bin/linux-<arch>/e2e.test (mise run build:linux), sidecar image (default
#        debian:trixie-slim@<Dockerfile BASE_DIGEST>; pulled by docker if absent).
# Output: $LENSDOCK_E2E_OUT/<TestName>/..., sidecar-tests.log and, when TestTX03 ran, tx03-xclip-after.txt
#         (the X CLIPBOARD selection after the HUMAN lease ended; it must be empty).

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

REGEX=${1:-^(TestCT04|TestTX01|TestTX02|TestTX03|TestSpike03CutText|TestRT03Viewers)$}

wait_ready 90 || die "runtime is not ready at $API_URL"
log "sidecar tests $REGEX using image $SIDECAR_IMAGE; evidence in $OUT"
rc=0
sidecar_run "$REGEX" 2>&1 | tee "$OUT/sidecar-tests.log" || rc=${PIPESTATUS[0]}

if [[ "TestTX03" =~ $REGEX ]]; then
  log "checking the X CLIPBOARD selection after the HUMAN lease ended"
  # Empty selection: xclip exits non-zero ("target STRING not available") or prints nothing.
  xout=$(docker exec -u 10001 -e DISPLAY=:99 -e XAUTHORITY=/run/lensdock/browser/Xauthority \
    "$CONTAINER" xclip -selection clipboard -o 2>"$OUT/tx03-xclip-after.stderr") && xrc=0 || xrc=$?
  printf '%s' "$xout" >"$OUT/tx03-xclip-after.txt"
  printf 'xclip exit status %s, %s bytes of clipboard text\n' "$xrc" "${#xout}" >"$OUT/tx03-xclip-after.status"
  cat "$OUT/tx03-xclip-after.status"
  if [ "${#xout}" -ne 0 ]; then
    log "FAIL: clipboard still holds text after the HUMAN lease ended"
    rc=1
  fi
fi
record_hashes "$OUT"
exit "$rc"
