#!/usr/bin/env bash
# Run the host-side e2e tests against the running runtime (published API 127.0.0.1:18080 + docker exec):
# TestRT02 (sandbox), TestRT03 (agent side closed, browser untouched), TestCT03 (stale leases,
# TTL, generation change by killing Chromium, real bridge exit code 3), TestCT10 (status table).
#
# Usage: tests/e2e/run-host.sh [TEST_REGEX]       default '^(TestRT02|TestRT03|TestCT10|TestCT03)$'
# Needs: runtime up and ready; .lensdock/operator-token; bin/lensdock and bin/e2e.test for this OS
#        (macOS: mise run build e2e:build-host; Linux host: bin/linux-<arch>/{lensdock,e2e.test}).
# Output: $LENSDOCK_E2E_OUT/<TestName>/... plus evidence.sha256 and host-tests.log.

# shellcheck source=lib.sh disable=SC1091
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

REGEX=${1:-^(TestRT02|TestRT03|TestCT10|TestCT03)$}

wait_ready 90 || die "runtime is not ready at $API_URL"
log "running $REGEX; evidence in $OUT"
rc=0
run_host_tests "$REGEX" 2>&1 | tee "$OUT/host-tests.log" || rc=${PIPESTATUS[0]}
log "e2e.test exit status $rc"
exit "$rc"
