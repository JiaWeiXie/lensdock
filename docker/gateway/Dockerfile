# syntax=docker/dockerfile:1
ARG NODE_IMAGE=node:24.22.1-trixie-slim@sha256:43b9ba1142b8dcf33fa8f922ca1be85714adc444e0586c7cf7726daaeabc1bea
ARG GO_IMAGE=golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4
ARG BASE_DIGEST=sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2
ARG DEBIAN_IMAGE=debian:trixie-slim@${BASE_DIGEST}
ARG APT_MIN_VERSION=3.0.4
ARG ZLIB_MIN_VERSION=1:1.3.dfsg+really1.3.1-1+deb13u1
FROM --platform=$BUILDPLATFORM ${NODE_IMAGE} AS webbuild
RUN npm install --global pnpm@11.28.8
WORKDIR /src
COPY apps/web/package.json apps/web/pnpm-lock.yaml apps/web/.npmrc ./
RUN --mount=type=cache,target=/root/.local/share/pnpm/store pnpm install --frozen-lockfile
COPY apps/web ./
RUN pnpm build

FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS gobuild
ARG TARGETOS
ARG TARGETARCH
ENV GOTOOLCHAIN=local CGO_ENABLED=0 GOFLAGS=-mod=readonly
WORKDIR /src
COPY go.mod go.su[m] ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" go build -trimpath -ldflags="-s -w" -o /out/lensdock-gateway ./cmd/gateway

FROM ${DEBIAN_IMAGE}
ARG BASE_DIGEST
ARG APT_MIN_VERSION
ARG ZLIB_MIN_VERSION
ARG REVISION=unknown
LABEL org.opencontainers.image.title="lensdock-gateway" \
      org.opencontainers.image.base.name="debian:trixie-slim" \
      org.opencontainers.image.base.digest="${BASE_DIGEST}" \
      org.opencontainers.image.revision="${REVISION}"
RUN set -eux; \
    apt-get update; \
    apt-get install --yes --no-install-recommends "zlib1g=${ZLIB_MIN_VERSION}"; \
    rm -rf /var/lib/apt/lists/*; \
    for package in apt libapt-pkg7.0; do \
      version="$(dpkg-query -W -f='${Version}' "$package")"; \
      dpkg --compare-versions "$version" ge "$APT_MIN_VERSION"; \
    done; \
    version="$(dpkg-query -W -f='${Version}' zlib1g)"; \
    dpkg --compare-versions "$version" ge "$ZLIB_MIN_VERSION"; \
    groupadd --gid 10000 lensdock; \
    useradd --uid 10000 --gid 10000 --no-create-home --home-dir /nonexistent --shell /usr/sbin/nologin lensdock; \
    install -d -m 0700 -o 10000 -g 10000 /data/db /data/private /data/artifacts
COPY --from=gobuild --chmod=0755 /out/lensdock-gateway /usr/local/bin/lensdock-gateway
COPY --from=webbuild /src/dist /app/web
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=5s --start-period=15s --retries=3 CMD ["/usr/local/bin/lensdock-gateway", "healthcheck"]
# The launcher reads root-only credentials, then permanently drops to UID/GID 10000
# before opening the database or accepting HTTP. No privileged worker survives.
ENTRYPOINT ["/usr/local/bin/lensdock-gateway"]
CMD ["serve", "--config", "/run/lensdock/gateway.json"]
