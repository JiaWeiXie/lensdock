#!/usr/bin/env bash
# Create the GitHub release v<version> with its assets, or confirm that it already exists.
#
# Usage: tools/release/publish-release.sh --version X.Y.Z --sha COMMIT --assets DIR --notes FILE
# Environment: GH_TOKEN (contents: write) and GITHUB_REPOSITORY (OWNER/REPO).
#
# The release is first created as a draft and filled with every file of DIR, then published in
# one step, so nobody ever sees a release with missing assets. Publishing creates the tag
# v<version> at COMMIT (a lightweight tag made by the API, so it triggers no other workflow).
# Re-running after a partial failure is safe: a leftover draft is replaced, and a published
# release is accepted only when its tag points at COMMIT and it carries the same asset names.
set -euo pipefail

die() { printf 'publish-release.sh: %s\n' "$*" >&2; exit 2; }

version="" sha="" assets="" notes=""
while (($#)); do
  (($# >= 2)) || die "$1 needs a value"
  case $1 in
    --version) version=$2 ;;
    --sha) sha=$2 ;;
    --assets) assets=$2 ;;
    --notes) notes=$2 ;;
    *) die "unknown argument: $1" ;;
  esac
  shift 2
done
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must look like 1.2.3 (no leading v)"
[[ $sha =~ ^[0-9a-f]{40}$ ]] || die "--sha must be a full commit hash"
[[ -d $assets && -f $notes ]] || die "--assets DIR and --notes FILE are required"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GH_TOKEN:?GH_TOKEN is required}"
command -v gh >/dev/null 2>&1 || die "gh is required"

tag="v$version"
expected=$(cd "$assets" && printf '%s\n' * | sort | tr '\n' ' ')
[[ $expected == *"SHA256SUMS"* && $expected == *"install.sh"* ]] || die "$assets must contain SHA256SUMS and install.sh"

tag_commit() { gh api "repos/$GITHUB_REPOSITORY/commits/$tag" --jq .sha 2>/dev/null || true; }
release_assets() { gh release view "$tag" --repo "$GITHUB_REPOSITORY" --json assets --jq '[.assets[].name] | sort | join(" ") + " "'; }

if gh release view "$tag" --repo "$GITHUB_REPOSITORY" >/dev/null 2>&1; then
  if [[ $(gh release view "$tag" --repo "$GITHUB_REPOSITORY" --json isDraft --jq .isDraft) == true ]]; then
    printf 'replacing leftover draft release %s\n' "$tag" >&2
    gh release delete "$tag" --repo "$GITHUB_REPOSITORY" --yes
  else
    [[ $(tag_commit) == "$sha" ]] || die "release $tag exists and its tag points at $(tag_commit), not $sha"
    [[ $(release_assets) == "$expected" ]] || die "release $tag exists with different assets: $(release_assets)"
    printf 'release %s is already published with the expected assets\n' "$tag" >&2
    exit 0
  fi
fi

files=()
for f in "$assets"/*; do files+=("$f"); done
gh release create "$tag" --repo "$GITHUB_REPOSITORY" --draft --target "$sha" \
  --title "LensDock $tag" --notes-file "$notes" "${files[@]}"
[[ $(release_assets) == "$expected" ]] || die "draft $tag has different assets than $assets: $(release_assets)"
gh release edit "$tag" --repo "$GITHUB_REPOSITORY" --draft=false --latest
[[ $(tag_commit) == "$sha" ]] || die "published tag $tag points at $(tag_commit), not $sha"
printf 'published %s\n' "$tag" >&2
