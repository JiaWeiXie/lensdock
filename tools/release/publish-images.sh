#!/usr/bin/env bash
# Publish the multi-platform runtime and gateway images from the per-platform digests that
# the native build jobs pushed untagged.
#
# Usage:
#   tools/release/publish-images.sh tags --version X.Y.Z --repository OWNER/REPO --sha COMMIT \
#       --digests DIR --out images.json
#       Creates <version>, <major.minor> and sha-<7> for both images as one index per image
#       (linux/amd64 + linux/arm64, with both platforms' SBOM and provenance attestations)
#       and writes the final digests to images.json. DIR holds digests-amd64.json and
#       digests-arm64.json as written by the build jobs.
#   tools/release/publish-images.sh latest --images images.json
#       Points `latest` at exactly the digests recorded in images.json. Run it last, after the
#       GitHub release exists.
#
# Images are $REGISTRY/<owner>/<repo>/runtime and .../gateway (REGISTRY defaults to ghcr.io,
# the repository path is lowercased). Needs docker buildx and jq, logged in with push access.
set -euo pipefail

die() { printf 'publish-images.sh: %s\n' "$*" >&2; exit 2; }

for cmd in docker jq; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing required command: $cmd"
done
(($# >= 1)) || die "usage: publish-images.sh tags|latest [flags]"
mode=$1
shift
version="" repository="" sha="" digests="" out="" images=""
while (($#)); do
  (($# >= 2)) || die "$1 needs a value"
  case $1 in
    --version) version=$2 ;;
    --repository) repository=$2 ;;
    --sha) sha=$2 ;;
    --digests) digests=$2 ;;
    --out) out=$2 ;;
    --images) images=$2 ;;
    *) die "unknown argument: $1" ;;
  esac
  shift 2
done
registry=${REGISTRY:-ghcr.io}

# digest_of REF prints the digest of the image index REF points at.
digest_of() { docker buildx imagetools inspect "$1" --format '{{json .Manifest}}' | jq -r .digest; }

case $mode in
  tags)
    [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "--version must look like 1.2.3 (no leading v)"
    [[ $repository =~ ^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]] || die "--repository must be OWNER/REPO"
    [[ $sha =~ ^[0-9a-f]{40}$ ]] || die "--sha must be a full commit hash"
    [[ -d $digests && -n $out ]] || die "--digests DIR and --out FILE are required"
    base="$registry/$(printf '%s' "$repository" | tr '[:upper:]' '[:lower:]')"
    result=$(jq -n --arg version "$version" '{version: $version}')
    for name in runtime gateway; do
      sources=()
      for arch in amd64 arm64; do
        d=$(jq -er --arg arch "$arch" --arg name "$name" 'select(.arch == $arch) | .[$name]' "$digests/digests-$arch.json") ||
          die "digests-$arch.json has no $name digest for $arch"
        [[ $d =~ ^sha256:[0-9a-f]{64}$ ]] || die "invalid $name digest for $arch: $d"
        sources+=("$base/$name@$d")
      done
      docker buildx imagetools create \
        --annotation "index:org.opencontainers.image.source=https://github.com/$repository" \
        --annotation "index:org.opencontainers.image.revision=$sha" \
        --annotation "index:org.opencontainers.image.version=$version" \
        -t "$base/$name:$version" -t "$base/$name:${version%.*}" -t "$base/$name:sha-${sha:0:7}" \
        "${sources[@]}"
      # The result must carry both platforms and an attestation manifest for each.
      docker buildx imagetools inspect "$base/$name:$version" --raw | jq -e '
        ([.manifests[] | select(.platform.os == "linux") | .platform.architecture] | sort) == ["amd64", "arm64"]
        and ([.manifests[] | select(.annotations["vnd.docker.reference.type"] == "attestation-manifest")] | length) == 2' >/dev/null ||
        die "$name:$version is not a linux/amd64 + linux/arm64 index with two attestation manifests"
      digest=$(digest_of "$base/$name:$version")
      for tag in "${version%.*}" "sha-${sha:0:7}"; do
        [[ $(digest_of "$base/$name:$tag") == "$digest" ]] || die "$name:$tag does not point at $digest"
      done
      result=$(jq --arg name "$name" --arg image "$base/$name" --arg digest "$digest" '.[$name] = {image: $image, digest: $digest}' <<<"$result")
    done
    printf '%s\n' "$result" >"$out"
    ;;
  latest)
    [[ -f $images ]] || die "--images FILE is required"
    for name in runtime gateway; do
      image=$(jq -er --arg name "$name" '.[$name].image' "$images")
      digest=$(jq -er --arg name "$name" '.[$name].digest' "$images")
      [[ $digest =~ ^sha256:[0-9a-f]{64}$ ]] || die "invalid $name digest: $digest"
      docker buildx imagetools create -t "$image:latest" "$image@$digest"
      [[ $(digest_of "$image:latest") == "$digest" ]] || die "$image:latest does not point at $digest"
    done
    ;;
  *) die "unknown mode: $mode" ;;
esac
