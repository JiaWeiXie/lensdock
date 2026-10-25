# syntax=docker/dockerfile:1
#
# LensDock browser runtime image. Build context is the repository root:
#   docker buildx build -f docker/browser/Dockerfile .
#
# Both base images are pinned to multi-arch index digests. Refresh them together with
# mise.toml / go.mod (see docs/UPDATE-RUNBOOK.md); tests/e2e/check-pins.sh guards the Go pin.

ARG GO_IMAGE=golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4
ARG BASE_DIGEST=sha256:62bd423aeee912639ca8ee681c2b762cb3e25137e0a92b20be455898a9f034ad
ARG DEBIAN_IMAGE=debian:trixie-slim@${BASE_DIGEST}

# ---------------------------------------------------------------------------
# gobuild: static runtime binary, cross-compiled natively for the target platform.
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS gobuild
ARG TARGETOS
ARG TARGETARCH
ENV GOTOOLCHAIN=local \
    CGO_ENABLED=0 \
    GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
    go build -trimpath -ldflags="-s -w" -o /out/lensdock-runtime ./cmd/runtime

# ---------------------------------------------------------------------------
# os-base: Debian packages, locales, users, fonts. Contains no LensDock code, so
# `--target os-base` builds only the OS layer.
# ---------------------------------------------------------------------------
FROM ${DEBIAN_IMAGE} AS os-base

RUN --mount=type=bind,source=docker/browser/packages.txt,target=/run/lensdock-packages.txt \
    set -eux; \
    export DEBIAN_FRONTEND=noninteractive; \
    apt-get update; \
    grep -Ev '^[[:space:]]*(#|$)' /run/lensdock-packages.txt \
      | xargs apt-get install -y --no-install-recommends; \
    mkdir -p /usr/share/lensdock; \
    dpkg-query -W -f='${Package}\t${Version}\t${Architecture}\n' > /usr/share/lensdock/dpkg-manifest.tsv; \
    rm -rf /var/lib/apt/lists/*; \
    test ! -e /usr/lib/chromium/chrome-sandbox

RUN set -eux; \
    sed -i -E 's/^# *(zh_TW\.UTF-8 UTF-8)/\1/; s/^# *(en_US\.UTF-8 UTF-8)/\1/' /etc/locale.gen; \
    locale-gen; \
    groupadd --gid 10000 lensdock; \
    useradd --uid 10000 --gid 10000 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin lensdock; \
    groupadd --gid 10001 browser; \
    useradd --uid 10001 --gid 10001 --no-create-home --home-dir /home/browser --shell /usr/sbin/nologin browser; \
    install -d -m 0755 -o root -g root /data; \
    install -d -m 0700 -o 10001 -g 10001 /data/profile /data/downloads; \
    install -d -m 0700 -o 10000 -g 10000 /data/runtime

COPY docker/browser/fontconfig.reference.conf /etc/fonts/local.conf
RUN set -eux; \
    fc-cache -f; \
    fc-list | grep -q 'Noto Sans CJK TC'; \
    fc-list | grep -q 'Noto Color Emoji'

# ---------------------------------------------------------------------------
# runtime: os-base plus the LensDock supervisor and the kiosk window-manager config.
# ---------------------------------------------------------------------------
FROM os-base AS runtime
ARG BASE_DIGEST
ARG REVISION=unknown

LABEL org.opencontainers.image.title="lensdock-runtime" \
      org.opencontainers.image.base.name="debian:trixie-slim" \
      org.opencontainers.image.base.digest="${BASE_DIGEST}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=gobuild --chmod=0755 /out/lensdock-runtime /usr/local/bin/lensdock-runtime
RUN install -d -m 0755 /etc/lensdock/openbox /etc/chromium/policies/managed
COPY --chmod=0644 docker/browser/openbox/rc.xml docker/browser/openbox/menu.xml /etc/lensdock/openbox/
# Keep browser-initiated downloads in the persistent transfer volume after CDP clients detach.
COPY --chmod=0644 docker/browser/downloads-policy.json /etc/chromium/policies/managed/lensdock-downloads.json

EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=60s --retries=3 \
    CMD ["/usr/local/bin/lensdock-runtime", "healthcheck"]
ENTRYPOINT ["/usr/bin/tini", "-g", "--", "/usr/local/bin/lensdock-runtime", "supervise"]
