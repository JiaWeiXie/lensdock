#!/usr/bin/env bash
# Compute the next release version from Conventional Commits, or verify a release tag.
#
# Usage:
#   tools/release/version.sh                    print the version the next release tag must have
#   tools/release/version.sh --check-tag vX.Y.Z verify that a pushed tag may be released
# Needs: git (full history and tags) and git-cliff on PATH; configuration is cliff.toml.
#
# Releases are cut by pushing a tag (see .github/workflows/release.yml). Nothing is
# released when commits merge. Run the first form to find out which tag to push.
#
# Prints key=value lines on stdout and appends them to $GITHUB_OUTPUT when it is set:
#   should_release  true | false  (false: nothing to release; --check-tag exits non-zero instead)
#   version         X.Y.Z (no leading v); empty when there is nothing to release
#   tag             vX.Y.Z
#   major_minor     X.Y
#   previous_tag    latest earlier release tag, empty before the first release
#   reason          human-readable explanation
#
# The first release is cliff.toml's bump.initial_tag (0.3.0). Afterwards `feat` bumps the
# minor version, any other commit bumps the patch version, and breaking changes bump the
# minor version while the major version is 0.
#
# --check-tag requires that the tag
#   - is a strict stable tag vX.Y.Z that points at the checked-out commit (HEAD); on a
#     tag-triggered run the checkout is the tag, so this only guards manual runs,
#   - points at a commit that is an ancestor of origin/main, not necessarily its tip
#     (override: RELEASE_MAIN_REF),
#   - is newer than the latest earlier release tag, which must be an ancestor of HEAD, and
#   - is exactly the version computed from the commits since that tag.
# Tags are immutable: to fix a bad release, push the next patch tag.
set -euo pipefail

die() { printf 'version.sh: %s\n' "$*" >&2; exit 1; }

mode=next check_tag=""
case ${1:-} in
  "") ;;
  --check-tag)
    (($# == 2)) || die "usage: version.sh [--check-tag vX.Y.Z]"
    mode=check check_tag=$2
    ;;
  *) die "usage: version.sh [--check-tag vX.Y.Z]" ;;
esac

cd "$(dirname "${BASH_SOURCE[0]}")/../.."
command -v git >/dev/null 2>&1 || die "git is required"
command -v git-cliff >/dev/null 2>&1 || die "git-cliff is required"
[[ $(git rev-parse --is-shallow-repository) == false ]] || die "full history is required (checkout with fetch-depth: 0)"

tag_re='^v[0-9]+\.[0-9]+\.[0-9]+$'
if [[ $mode == check ]]; then
  [[ $check_tag =~ $tag_re ]] || die "tag '$check_tag' is not a strict release tag vX.Y.Z"
fi

previous=""
while IFS= read -r t; do
  [[ $t == "$check_tag" ]] && continue
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
  if [[ $mode == next && $(git rev-parse "$previous^{commit}") == "$(git rev-parse HEAD)" ]]; then
    emit false "" "HEAD is already released as $previous"
    exit 0
  fi
fi

if [[ $mode == check ]]; then
  git rev-parse --verify --quiet "refs/tags/$check_tag" >/dev/null || die "tag $check_tag does not exist"
  [[ $(git rev-parse "$check_tag^{commit}") == "$(git rev-parse HEAD)" ]] || die "tag $check_tag does not point at the checked-out commit"
  main_ref=${RELEASE_MAIN_REF:-origin/main}
  git rev-parse --verify --quiet "$main_ref^{commit}" >/dev/null || die "$main_ref not found; fetch the full history"
  git merge-base --is-ancestor HEAD "$main_ref" || die "tag $check_tag is not on $main_ref; only reviewed commits on the main branch may be released"
  if [[ -n $previous ]]; then
    [[ ${previous#v} != "${check_tag#v}" && $(printf '%s\n%s\n' "${previous#v}" "${check_tag#v}" | sort -V | tail -n1) == "${check_tag#v}" ]] \
      || die "tag $check_tag is not newer than the latest earlier release $previous"
  fi
fi

# Compute the bump as if the tag under check did not exist, so its own commits count as unreleased.
cliff_args=(--config cliff.toml --bumped-version)
[[ $mode == check ]] && cliff_args+=(--ignore-tags "^${check_tag//./\\.}\$")
bumped=$(git-cliff "${cliff_args[@]}") || die "git-cliff could not compute the next version"
bumped=${bumped#v}
[[ $bumped =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "git-cliff returned an unexpected version: '$bumped'"

if [[ $mode == check ]]; then
  [[ $bumped == "${check_tag#v}" ]] || die "tag $check_tag does not match the version computed from the commits since ${previous:-the start}: v$bumped"
  emit true "$bumped" "tag $check_tag matches the computed version"
  exit 0
fi

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
