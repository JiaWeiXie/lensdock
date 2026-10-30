#!/usr/bin/env bash
# Tests for tools/release/version.sh (next version, tag verification) and the
# release-notes range of tools/release/notes.sh when the release tag already exists.
# Builds throwaway git repositories; needs git and git-cliff. Touches nothing else.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
command -v git-cliff >/dev/null 2>&1 || { echo "version_test.sh: git-cliff is required" >&2; exit 2; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

pass=0 failed=0
ok() { pass=$((pass + 1)); printf 'ok   %s\n' "$1"; }
bad() { failed=$((failed + 1)); printf 'FAIL %s\n' "$1" >&2; }

new_repo() {
  local dir=$1
  mkdir -p "$dir/tools/release"
  git init -q -b main "$dir"
  cp "$root/cliff.toml" "$dir/cliff.toml"
  cp "$root/tools/release/version.sh" "$root/tools/release/notes.sh" "$dir/tools/release/"
  git -C "$dir" config user.name test
  git -C "$dir" config user.email test@example.invalid
  git -C "$dir" config commit.gpgsign false
  git -C "$dir" config tag.gpgsign false
  git -C "$dir" add -A
  git -C "$dir" commit -q -m "chore: scaffold"
}
commit() { git -C "$1" commit -q --allow-empty -m "$2"; }

# run DIR ARGS...: prints stdout, sets $status; the main ref is the local branch.
run() {
  local dir=$1
  shift
  status=0
  out=$(cd "$dir" && RELEASE_MAIN_REF=main bash tools/release/version.sh "$@" 2>"$tmp/err") || status=$?
}
expect_ok() {
  local name=$1 want=$2
  if [[ $status -eq 0 && $out == *"version=$want"* ]]; then ok "$name"; else bad "$name (status $status, out '$out', err '$(cat "$tmp/err")')"; fi
}
expect_fail() {
  local name=$1 pattern=$2
  if [[ $status -ne 0 && $(cat "$tmp/err") == *$pattern* ]]; then ok "$name"; else bad "$name (status $status, err '$(cat "$tmp/err")')"; fi
}

# First release: bump.initial_tag.
r="$tmp/first"
new_repo "$r"
commit "$r" "feat: first feature"
run "$r"
expect_ok "first release is the initial tag" "0.3.0"
git -C "$r" tag v0.3.0
run "$r" --check-tag v0.3.0
expect_ok "first tag verifies" "0.3.0"

# Normal flow after a release.
r="$tmp/flow"
new_repo "$r"
commit "$r" "feat: first feature"
git -C "$r" tag v0.3.0
commit "$r" "fix: patch"
run "$r"
expect_ok "fix bumps the patch version" "0.3.1"
git -C "$r" tag v0.3.1
run "$r" --check-tag v0.3.1
expect_ok "matching patch tag verifies" "0.3.1"
run "$r"
if [[ $status -eq 0 && $out == *"should_release=false"* ]]; then ok "next on a released HEAD says nothing to release"; else bad "next on a released HEAD (status $status, out '$out')"; fi

commit "$r" "feat: another feature"
git -C "$r" tag v0.4.0
run "$r" --check-tag v0.4.0
expect_ok "feat tag bumps the minor version" "0.4.0"

commit "$r" "fix!: breaking fix"
git -C "$r" tag v0.5.0
run "$r" --check-tag v0.5.0
expect_ok "breaking change bumps the minor version while major is 0" "0.5.0"

# Wrong version for the commits.
commit "$r" "fix: small"
git -C "$r" tag v0.9.0
run "$r" --check-tag v0.9.0
expect_fail "tag that does not match the computed version is rejected" "does not match the version computed"
git -C "$r" tag -d v0.9.0 >/dev/null

# Tag not at HEAD.
git -C "$r" tag v0.5.1 HEAD~1
run "$r" --check-tag v0.5.1
expect_fail "tag that is not the checked-out commit is rejected" "does not point at the checked-out commit"
git -C "$r" tag -d v0.5.1 >/dev/null

# Non-strict tags.
for t in v1.2 v0.5.1-rc1 0.5.1 v0.5.1+build; do
  run "$r" --check-tag "$t"
  expect_fail "non-strict tag $t is rejected" "not a strict release tag"
done

# Not newer than the previous release (v0.5.0 exists; HEAD carries a lower version).
commit "$r" "fix: later"
git -C "$r" tag v0.4.9
run "$r" --check-tag v0.4.9
expect_fail "tag lower than an existing release is rejected" "not newer than the latest earlier release"
git -C "$r" tag -d v0.4.9 >/dev/null

# Tag on a commit that is not on main.
git -C "$r" checkout -q -b side
commit "$r" "fix: unreviewed"
git -C "$r" tag v0.5.1
run "$r" --check-tag v0.5.1
expect_fail "tag outside main is rejected" "is not on main"
git -C "$r" tag -d v0.5.1 >/dev/null
git -C "$r" checkout -q main

# Missing tag.
run "$r" --check-tag v0.5.2
expect_fail "missing tag is rejected" "does not exist"

# A tag older than an existing release is rejected even when it matches no later release.
r="$tmp/older"
new_repo "$r"
commit "$r" "feat: a"
git -C "$r" tag v0.3.0
commit "$r" "fix: b"
git -C "$r" tag v0.3.1
commit "$r" "fix: c"
run "$r" --check-tag v0.3.1
expect_fail "re-checking an earlier release tag at a later commit is rejected" "does not point at the checked-out commit"

# Release notes: the same text before and after the tag exists.
r="$tmp/notes"
new_repo "$r"
commit "$r" "feat: a"
git -C "$r" tag v0.3.0
commit "$r" "feat: shiny thing"
commit "$r" "fix: crash"
before=$(cd "$r" && bash tools/release/notes.sh changelog --version 0.4.0)
git -C "$r" tag v0.4.0
after=$(cd "$r" && bash tools/release/notes.sh changelog --version 0.4.0)
if [[ $before == "$after" && $after == *"Shiny thing"* && $after == *"Crash"* && $after != *"scaffold"* ]]; then
  ok "release notes are identical before and after the tag exists"
else
  bad "release notes differ: before '$before' / after '$after'"
fi

printf '%d passed, %d failed\n' "$pass" "$failed"
((failed == 0))
