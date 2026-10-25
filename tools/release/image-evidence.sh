#!/usr/bin/env bash
# Extract and gate the supply-chain evidence of one pushed per-platform image.
#
# Usage: tools/release/image-evidence.sh NAME IMAGE_REF OUT_DIR PLATFORM
#   NAME       runtime | gateway (output file prefix)
#   IMAGE_REF  registry/repository@sha256:<digest> of the image index pushed by the build
#   OUT_DIR    receives NAME.sbom.spdx.json, NAME.provenance.slsa.json and NAME.trivy.json
#   PLATFORM   linux/amd64 | linux/arm64, matching the image being scanned
#
# Fails when the BuildKit SBOM or provenance attestation is missing or empty, or when Trivy
# finds a HIGH or CRITICAL vulnerability that has a fixed version. Vulnerabilities without a
# fix, and accepted exceptions listed in .github/trivyignore, do not block. Needs docker
# (buildx), jq and trivy, and a registry login that can pull IMAGE_REF.
set -euo pipefail

die() { printf 'image-evidence.sh: %s\n' "$*" >&2; exit 1; }

(($# == 4)) || die "usage: image-evidence.sh NAME IMAGE_REF OUT_DIR PLATFORM"
name=$1 ref=$2 out=$3 platform=$4
[[ $platform == linux/amd64 || $platform == linux/arm64 ]] || die "unsupported scan platform: $platform"
[[ $ref =~ @sha256:[0-9a-f]{64}$ ]] || die "IMAGE_REF must be pinned by digest: $ref"
for cmd in docker jq trivy; do
  command -v "$cmd" >/dev/null 2>&1 || die "missing required command: $cmd"
done
cd "$(dirname "${BASH_SOURCE[0]}")/../.."
mkdir -p "$out"

docker buildx imagetools inspect "$ref" --format '{{json .SBOM.SPDX}}' >"$out/$name.sbom.spdx.json"
jq -e '.spdxVersion != null and (.packages | length) > 0' "$out/$name.sbom.spdx.json" >/dev/null ||
  die "$name: the image has no SPDX SBOM attestation with packages"

docker buildx imagetools inspect "$ref" --format '{{json .Provenance.SLSA}}' >"$out/$name.provenance.slsa.json"
jq -e '.buildDefinition != null and .runDetails != null' "$out/$name.provenance.slsa.json" >/dev/null ||
  die "$name: the image has no SLSA provenance attestation"

# Full report for the evidence archive, then the gate. The second run reuses the
# vulnerability database and the layer cache of the first.
trivy image --platform "$platform" --quiet --scanners vuln --format json --output "$out/$name.trivy.json" --exit-code 0 "$ref"
trivy image --platform "$platform" --quiet --scanners vuln --severity HIGH,CRITICAL --ignore-unfixed \
  --ignorefile .github/trivyignore --skip-db-update --skip-java-db-update \
  --format table --exit-code 1 "$ref"
printf '%s: SBOM, provenance and vulnerability gate passed for %s\n' "$name" "$ref"
