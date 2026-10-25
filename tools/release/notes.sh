#!/usr/bin/env bash
# Generate GitHub release notes.
#
# Usage:
#   tools/release/notes.sh changelog --version X.Y.Z
#       Conventional-commit changelog for the commits since the previous release tag
#       (all commits for the first release). Needs git-cliff and full git history.
#   tools/release/notes.sh compose --version X.Y.Z --changelog FILE --images images.json
#       Final release notes: the changelog, the asset list and the exact image digests.
#       images.json is written by tools/release/publish-images.sh. Needs jq only.
set -euo pipefail

die() { printf 'notes.sh: %s\n' "$*" >&2; exit 2; }

(($# >= 1)) || die "usage: notes.sh changelog|compose [flags]"
mode=$1
shift
version="" changelog_file="" images_file=""
while (($#)); do
  case $1 in
    --version) (($# >= 2)) || die "--version needs a value"; version=$2; shift 2 ;;
    --changelog) (($# >= 2)) || die "--changelog needs a value"; changelog_file=$2; shift 2 ;;
    --images) (($# >= 2)) || die "--images needs a value"; images_file=$2; shift 2 ;;
    *) die "unknown argument: $1" ;;
  esac
done
[[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must look like 1.2.3 (no leading v)"

case $mode in
  changelog)
    cd "$(dirname "${BASH_SOURCE[0]}")/../.."
    command -v git-cliff >/dev/null 2>&1 || die "git-cliff is required"
    out=$(git-cliff --config cliff.toml --unreleased --tag "v$version" --strip header) || die "git-cliff failed"
    # Trim surrounding blank lines.
    out=${out#"${out%%[![:space:]]*}"}
    out=${out%"${out##*[![:space:]]}"}
    if [[ -z $out ]]; then
      out="No commits were recorded for this release."
    fi
    printf '%s\n' "$out"
    ;;
  compose)
    [[ -f $changelog_file ]] || die "--changelog FILE is required"
    [[ -f $images_file ]] || die "--images FILE is required"
    command -v jq >/dev/null 2>&1 || die "jq is required"
    jq -e --arg v "$version" '.version == $v and (.runtime.image|type=="string") and (.gateway.image|type=="string")
      and (.runtime.digest|test("^sha256:[0-9a-f]{64}$")) and (.gateway.digest|test("^sha256:[0-9a-f]{64}$"))' \
      "$images_file" >/dev/null || die "$images_file does not describe version $version"
    runtime=$(jq -r '"\(.runtime.image):\(.version)@\(.runtime.digest)"' "$images_file")
    gateway=$(jq -r '"\(.gateway.image):\(.version)@\(.gateway.digest)"' "$images_file")
    printf "## What's changed\n\n"
    cat "$changelog_file"
    cat <<EOF

## Assets

| Asset | Purpose |
| --- | --- |
| \`lensdock_${version}_<darwin\|linux>_<amd64\|arm64>.tar.gz\` | \`lensdock\` CLI for that host, both Linux helper CLIs (\`bin/linux-<arch>/lensdock\`), \`docker/compose.yaml\`, the Chromium seccomp profile and the READMEs |
| \`SHA256SUMS\` | SHA-256 of every other asset: \`sha256sum -c SHA256SUMS --ignore-missing\` |
| \`install.sh\` | Installer that verifies \`SHA256SUMS\` before it extracts anything |

## Container images

Multi-platform (\`linux/amd64\`, \`linux/arm64\`), built natively on each platform. Also tagged \`${version%.*}\`, \`sha-<short commit>\` and \`latest\`.

| Image | Immutable reference |
| --- | --- |
| Runtime | \`${runtime}\` |
| Gateway | \`${gateway}\` |

## Verification and trust

- Each image index carries BuildKit SBOM and SLSA provenance attestations. Both images were scanned with Trivy (fixed HIGH and CRITICAL findings block a release) and a native fresh-install smoke test passed on each platform before these tags were published.
- The archives and \`SHA256SUMS\` are checksummed but **not signed**. This GitHub distribution is separate from the signed managed-update channel (\`lensdock release …\`); it is not a signed production release.
EOF
    ;;
  *) die "unknown mode: $mode" ;;
esac
