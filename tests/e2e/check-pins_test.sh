#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
checker="$root/tests/e2e/check-pins.sh"
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT

action='jdx/mise-action@b44d12e7d0d8ee7461bb893e1b1993500433185b'
sha='c72477ae6c3a8e7d817a322f43da70e4643e75ee5c21265f1c19c894273b7428'

write_valid_workflow() {
  local file=$1
  {
    echo 'jobs:'
    for job in check web package; do
      printf '  %s:\n    steps:\n' "$job"
      echo '      - uses: actions/checkout@pinned'
      echo '        with:'
      echo '          persist-credentials: false'
      printf '      - uses: %s\n' "$action"
      echo '        with:'
      echo '          version: 2026.10.8'
      printf '          sha256: %s\n' "$sha"
    done
  } >"$file"
}

expect_failure() {
  local name=$1 file=$2
  if "$checker" --check-mise-actions "$file" >"$tmp/$name.out" 2>"$tmp/$name.err"; then
    echo "check-pins-test: expected $name to fail" >&2
    exit 1
  fi
}

write_valid_workflow "$tmp/valid.yml"
"$checker" --check-mise-actions "$tmp/valid.yml"

awk -v action="$action" '
  !inserted && $0 == "      - uses: " action {
    print "      - uses: jdx/mise-action@v5.2.7"
    print "        with:"
    print "          version: latest"
    inserted = 1
  }
  { print }
' "$tmp/valid.yml" >"$tmp/extra-unpinned.yml"
expect_failure extra-unpinned "$tmp/extra-unpinned.yml"

awk -v sha="$sha" '
  !moved && $0 == "          persist-credentials: false" {
    print
    print "          sha256: " sha
    moved = 1
    next
  }
  !removed && $0 == "          sha256: " sha {
    removed = 1
    next
  }
  { print }
' "$tmp/valid.yml" >"$tmp/misassociated-sha.yml"
expect_failure misassociated-sha "$tmp/misassociated-sha.yml"

echo 'check-pins-test: ok'
