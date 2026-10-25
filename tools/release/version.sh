#!/usr/bin/env bash
# Decide whether HEAD is a release and compute its version from Conventional Commits.
#
# Usage: tools/release/version.sh
# Needs: git (full history and tags) and git-cliff on PATH; configuration is cliff.toml.
#
# Prints key=value lines on stdout and appends them to $GITHUB_OUTPUT when it is set:
#   should_release  true | false
#   version         X.Y.Z (no leading v); empty when there is nothing to release
#   tag             vX.Y.Z
#   major_minor     X.Y
#   previous_tag    latest existing release tag, empty before the first release
#   reason          human-readable explanation
#
# The first release is cliff.toml's bump.initial_tag (0.3.0). Afterwards `feat` bumps the
# minor version, any other commit bumps the patch version, and breaking changes bump the
# minor version while the major version is 0. Put "[skip release]" in the HEAD commit
# message to skip a release; later commits still roll that change into the next one.
# Failures are errors, never a silent "no release".
set -euo pipefail

die() { printf 'version.sh: %s\n' "$*" >&2; exit 1; }

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
command -v git >/dev/null 2>&1 || die "git is required"
command -v git-cliff >/dev/null 2>&1 || die "git-cliff is required"
[[ $(git rev-parse --is-shallow-repository) == false ]] || die "full history is required (checkout with fetch-depth: 0)"

tag_re='^v[0-9]+\.[0-9]+\.[0-9]+$'
previous=""
while IFS= read -r t; do
  if [[ $t =~ $tag_re ]]; then
    previous=$t
    break
  fi
done < <(git tag --list 'v[0-9]*' --sort=-v:refname)

emit() {
  local release=$1 version=$2 reason=$3 tag="" major_minor=""
  if [[ -n $version ]]; then
    tag="v$version"
    major_minor=${version%.*}
  fi
  local lines=(
    "should_release=$release"
    "version=$version"
    "tag=$tag"
    "major_minor=$major_minor"
    "previous_tag=$previous"
    "reason=$reason"
  )
  printf '%s\n' "${lines[@]}"
  if [[ -n ${GITHUB_OUTPUT:-} ]]; then
    printf '%s\n' "${lines[@]}" >>"$GITHUB_OUTPUT"
  fi
}

if [[ -n $previous ]]; then
  # A newer release that is not an ancestor means HEAD is stale or diverged (for
  # example a re-run of an old workflow); releasing it would move `latest` backwards.
  git merge-base --is-ancestor "$previous" HEAD || die "latest release $previous is not an ancestor of HEAD; refusing to release from a stale or diverged commit"
  if [[ $(git rev-parse "$previous^{commit}") == "$(git rev-parse HEAD)" ]]; then
    emit false "" "HEAD is already released as $previous"
    exit 0
  fi
fi

head_message=$(git log -1 --format=%B HEAD | tr '[:upper:]' '[:lower:]')
if [[ $head_message == *"[skip release]"* ]]; then
  emit false "" "the HEAD commit message contains [skip release]"
  exit 0
fi

bumped=$(git-cliff --config cliff.toml --bumped-version) || die "git-cliff could not compute the next version"
bumped=${bumped#v}
[[ $bumped =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "git-cliff returned an unexpected version: '$bumped'"

if [[ -n $previous && $bumped == "${previous#v}" ]]; then
  emit false "" "no commits since $previous"
  exit 0
fi
if git rev-parse --verify --quiet "refs/tags/v$bumped" >/dev/null; then
  die "tag v$bumped already exists but is not the latest release tag"
fi

if [[ -z $previous ]]; then
  emit true "$bumped" "first release"
else
  emit true "$bumped" "release $bumped follows $previous"
fi
