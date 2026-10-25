#!/usr/bin/env bash
# Merge native platform evidence, attach a REAL passed acceptance report,
# promote to the release channel, sign and verify. This script does not build,
# scan, test or publish anything; platform-evidence.sh and the acceptance jobs
# must have already produced the inputs.
set -euo pipefail

: "${RELEASE_ID:?RELEASE_ID is required}"
: "${AMD64_DIR:?AMD64_DIR is required (output of platform-evidence.sh)}"
: "${ARM64_DIR:?ARM64_DIR is required (output of platform-evidence.sh)}"
: "${TEST_REPORT:?TEST_REPORT is required (ReleaseTestReport JSON, passed=true)}"
: "${TEST_SUMMARY:?TEST_SUMMARY is required}"
: "${KEY_FILE:?KEY_FILE is required (0600 Ed25519 private key)}"
: "${LENSDOCK_BIN:?LENSDOCK_BIN is required (release CLI for this host)}"
: "${OUTPUT:?OUTPUT is required and must not exist}"

command -v jq >/dev/null 2>&1 || { printf 'missing required command: jq\n' >&2; exit 1; }

WORK=${WORK:-$(dirname "$OUTPUT")/.assemble-$RELEASE_ID}
if [[ -e "$WORK" || -e "$OUTPUT" ]]; then
  printf 'refusing existing WORK or OUTPUT (%s, %s)\n' "$WORK" "$OUTPUT" >&2
  exit 2
fi
mkdir -p "$WORK"
cleanup() { rm -rf "$WORK"; }
trap cleanup EXIT

"$LENSDOCK_BIN" release merge --output "$WORK/dev-merged" \
  "$AMD64_DIR/dev-release" "$ARM64_DIR/dev-release"

jq -s --arg releaseId "$RELEASE_ID" --arg report "$TEST_REPORT" --arg summary "$TEST_SUMMARY" \
  '{schemaVersion:1,releaseId:$releaseId,images:(map(.images)|add),tests:{report:$report,summary:$summary}}' \
  "$AMD64_DIR/evidence-index.partial.json" "$ARM64_DIR/evidence-index.partial.json" \
  >"$WORK/evidence-index.json"

"$LENSDOCK_BIN" release promote \
  --dev-release "$WORK/dev-merged" \
  --evidence-index "$WORK/evidence-index.json" \
  --output "$OUTPUT"
"$LENSDOCK_BIN" release sign --release "$OUTPUT" --key-file "$KEY_FILE"
"$LENSDOCK_BIN" release verify --release "$OUTPUT" --key-file "$KEY_FILE"

printf 'signed, verified release directory: %s\n' "$OUTPUT"
printf 'No image or release was published by assembly; the exact digest-pinned images were already pushed by the explicit per-platform evidence jobs.\n'
