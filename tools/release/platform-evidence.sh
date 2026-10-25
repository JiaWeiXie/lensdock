#!/usr/bin/env bash
# Build and collect REAL supply-chain evidence for one release architecture.
# This script intentionally requires a registry and `trivy`; it never substitutes
# made-up provenance, an empty scan, or a claimed test pass.
#
# Required environment:
#   RELEASE_ID                 immutable release id
#   ARCH                       amd64 or arm64
#   RUNTIME_REF                registry/repository:temporary-tag for this architecture
#   GIT_COMMIT                 exact 40-character commit
#   AGENT_BROWSER_VERSION      verified bundled/compatible version
#   NOVNC_VERSION              verified noVNC version
#   CLI_VERSION                LensDock CLI version
# Optional Gateway environment (all or none):
#   GATEWAY_REF GATEWAY_DOCKERFILE GATEWAY_VERSION GATEWAY_DB MIN_GATEWAY_DB
# Optional:
#   OUT (default out/release/$RELEASE_ID/$ARCH)
#   RUNTIME_DOCKERFILE (default docker/browser/Dockerfile)
#
# This command PUSHES the two temporary architecture refs to the configured
# registry because OCI attestations are not preserved by the Docker image
# store. Running it is an explicit release-manager action; normal builds and
# updates never publish anything.
set -euo pipefail

need() { command -v "$1" >/dev/null 2>&1 || { printf 'missing required command: %s\n' "$1" >&2; exit 1; }; }
need docker
need jq
need trivy
need mise

: "${RELEASE_ID:?RELEASE_ID is required}"
: "${ARCH:?ARCH is required (amd64 or arm64)}"
: "${RUNTIME_REF:?RUNTIME_REF is required and must name a registry}"
: "${GIT_COMMIT:?GIT_COMMIT is required}"
: "${AGENT_BROWSER_VERSION:?AGENT_BROWSER_VERSION is required}"
: "${NOVNC_VERSION:?NOVNC_VERSION is required}"
: "${CLI_VERSION:?CLI_VERSION is required}"
case "$ARCH" in amd64|arm64) ;; *) printf 'unsupported ARCH: %s\n' "$ARCH" >&2; exit 2;; esac
if [[ ! "$GIT_COMMIT" =~ ^[0-9a-f]{40}$ ]]; then printf 'GIT_COMMIT must be 40 lowercase hex characters\n' >&2; exit 2; fi

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
OUT=${OUT:-$ROOT/out/release/$RELEASE_ID/$ARCH}
RUNTIME_DOCKERFILE=${RUNTIME_DOCKERFILE:-$ROOT/docker/browser/Dockerfile}
cd "$ROOT"
if [[ -n $(git status --porcelain --untracked-files=normal) ]]; then
  printf 'release evidence requires a clean Git worktree before any output is generated\n' >&2
  exit 2
fi
mkdir -p "$OUT/evidence" "$OUT/bin"
if [[ $(uname -s) != Linux ]]; then
  printf 'platform evidence must run on native Linux CI runners (current OS: %s)\n' "$(uname -s)" >&2
  exit 2
fi
HOST_ARCH=$(uname -m)
case "$HOST_ARCH" in x86_64) HOST_ARCH=amd64;; aarch64|arm64) HOST_ARCH=arm64;; esac
if [[ "$HOST_ARCH" != "$ARCH" ]]; then
  printf 'this per-platform workflow must run on a %s host (current host: %s); use native amd64 and arm64 CI runners, then merge their outputs\n' "$ARCH" "$HOST_ARCH" >&2
  exit 2
fi

build_one() {
  local ref=$1 dockerfile=$2 metadata=$3
  docker buildx build "$ROOT" \
    --file "$dockerfile" \
    --platform "linux/$ARCH" \
    --tag "$ref" \
    --attest type=sbom \
    --attest type=provenance,mode=max,version=v1 \
    --metadata-file "$metadata" \
    --push
}

extract_one() {
  local name=$1 ref=$2 metadata=$3
  local index_digest exact raw platform_digest sbom predicate provenance scan
  index_digest=$(jq -er '.["containerimage.digest"]' "$metadata")
  exact="$ref@$index_digest"
  raw="$OUT/evidence/$name-index.json"
  docker buildx imagetools inspect "$exact" --raw >"$raw"
  platform_digest=$(jq -er --arg arch "$ARCH" '.manifests[] | select(.platform.os=="linux" and .platform.architecture==$arch) | .digest' "$raw")

  sbom="$OUT/evidence/$name.sbom.json"
  predicate="$OUT/evidence/$name.provenance.predicate.json"
  provenance="$OUT/evidence/$name.provenance.json"
  scan="$OUT/evidence/$name.trivy.json"
  docker buildx imagetools inspect "$exact" --format '{{json .SBOM.SPDX}}' >"$sbom"
  docker buildx imagetools inspect "$exact" --format '{{json .Provenance.SLSA}}' >"$predicate"
  jq -n --arg image "$ref" --arg digest "$platform_digest" --slurpfile predicate "$predicate" \
    '{_type:"https://in-toto.io/Statement/v1",predicateType:"https://slsa.dev/provenance/v1",subject:[{name:$image,digest:{sha256:($digest|sub("^sha256:";""))}}],predicate:$predicate[0]}' >"$provenance"
  trivy image --scanners vuln --format json --output "$scan" "$exact"

  jq -n --arg exact "$exact" --arg manifestDigest "$platform_digest" --arg sbom "$sbom" \
    --arg provenance "$provenance" --arg scan "$scan" --arg version "$(trivy --version | tr '\n' ' ')" \
    --arg scannedAt "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
    '{exact:$exact,evidence:{manifestDigest:$manifestDigest,sbom:$sbom,provenance:$provenance,scan:$scan,scanTool:"trivy",scanToolVersion:$version,scannedAt:$scannedAt}}'
}

RUNTIME_METADATA="$OUT/evidence/runtime-build-metadata.json"
build_one "$RUNTIME_REF" "$RUNTIME_DOCKERFILE" "$RUNTIME_METADATA"
RUNTIME_JSON=$(extract_one runtime "$RUNTIME_REF" "$RUNTIME_METADATA")
RUNTIME_EXACT=$(jq -r .exact <<<"$RUNTIME_JSON")
docker pull --platform "linux/$ARCH" "$RUNTIME_EXACT"

# The release CLI itself is a candidate verifier. Build it with the repository's
# pinned Go toolchain and commit its exact hash to the manifest.
CGO_ENABLED=0 GOOS=linux GOARCH="$ARCH" mise exec -- go build -trimpath -o "$OUT/bin/lensdock" ./cmd/lensdock

GATEWAY_ARGS=()
GATEWAY_EVIDENCE=null
if [[ -n "${GATEWAY_REF:-}" || -n "${GATEWAY_DOCKERFILE:-}" ]]; then
  : "${GATEWAY_REF:?GATEWAY_REF is required when building Gateway evidence}"
  : "${GATEWAY_DOCKERFILE:?GATEWAY_DOCKERFILE is required when building Gateway evidence}"
  : "${GATEWAY_VERSION:?GATEWAY_VERSION is required}"
  : "${GATEWAY_DB:?GATEWAY_DB is required}"
  : "${MIN_GATEWAY_DB:?MIN_GATEWAY_DB is required}"
  GATEWAY_METADATA="$OUT/evidence/gateway-build-metadata.json"
  build_one "$GATEWAY_REF" "$GATEWAY_DOCKERFILE" "$GATEWAY_METADATA"
  GATEWAY_JSON=$(extract_one gateway "$GATEWAY_REF" "$GATEWAY_METADATA")
  GATEWAY_EXACT=$(jq -r .exact <<<"$GATEWAY_JSON")
  docker pull --platform "linux/$ARCH" "$GATEWAY_EXACT"
  GATEWAY_EVIDENCE=$(jq -c .evidence <<<"$GATEWAY_JSON")
  GATEWAY_ARGS=(--gateway-image "$GATEWAY_EXACT" --gateway-version "$GATEWAY_VERSION" --gateway-db "$GATEWAY_DB" --min-gateway-db "$MIN_GATEWAY_DB")
fi

DEV_DIR="$OUT/dev-release"
"$OUT/bin/lensdock" release manifest --dev \
  --id "$RELEASE_ID" --output "$DEV_DIR" \
  --runtime-image "$RUNTIME_EXACT" --linux-cli "$OUT/bin/lensdock" \
  --git-commit "$GIT_COMMIT" --source-clean --agent-browser-version "$AGENT_BROWSER_VERSION" \
  --novnc-version "$NOVNC_VERSION" --cli-version "$CLI_VERSION" \
  "${GATEWAY_ARGS[@]}"

jq -n --arg releaseId "$RELEASE_ID" --arg arch "$ARCH" \
  --argjson runtime "$(jq -c .evidence <<<"$RUNTIME_JSON")" --argjson gateway "$GATEWAY_EVIDENCE" \
  '{schemaVersion:1,releaseId:$releaseId,images:{($arch):{runtime:$runtime}}} | if $gateway == null then . else .images[$arch].gateway=$gateway end' \
  >"$OUT/evidence-index.partial.json"

printf 'platform evidence ready: %s\n' "$OUT"
printf 'Not a release yet: merge architectures, attach a real passed acceptance report, promote, sign, then verify.\n'
