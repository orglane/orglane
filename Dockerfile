# syntax=docker/dockerfile:1
#
# Two images from one build (ADR 0019):
#
#   core   (default, last stage) the static binary with the web UI embedded,
#          on distroless, running as non-root. Build: make image (or
#          docker build -t orglane:dev .).
#   agent  (--target agent) the same binary plus the Docker CLI and the
#          Compose plugin, for `orglane agent` (managed services). Build:
#          docker build --target agent -t orglane-agent:dev .
#
# Releases build both for linux/amd64 and linux/arm64 (.github/workflows/
# images.yml): the web, Go and download stages run on the build platform, Go
# cross-compiles for TARGETPLATFORM, and the final stages have no RUN step.
#
# Base images are pinned by version and digest, the Docker tools by version
# and SHA-256. Bump both together; the Go and Node.js versions follow go.mod
# and .nvmrc.

# Web UI (web/): built once, architecture independent.
FROM --platform=$BUILDPLATFORM node:26.13.0-alpine3.24@sha256:c6ad9e9c1d80f6eb607c26b4b0a132ff7a2344fad1c78ea3baa2a7d4c8de6d94 AS web
WORKDIR /src
ENV COREPACK_ENABLE_DOWNLOAD_PROMPT=0 PNPM_HOME=/pnpm CI=true
# Node.js 25+ no longer bundles corepack. Same pin as .github/actions/setup;
# corepack then installs the pnpm version pinned in package.json.
RUN npm install --global --no-fund --no-audit corepack@0.36.0 && corepack enable pnpm
COPY package.json pnpm-lock.yaml pnpm-workspace.yaml tsconfig.base.json ./
COPY packages/ packages/
COPY web/ web/
RUN --mount=type=cache,target=/pnpm/store \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile && \
    pnpm --filter "@orglane/web..." run build

FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS build
WORKDIR /src
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-mod=readonly
COPY go.mod go.sum ./
COPY core/ core/
COPY demo/ demo/
COPY web/web.go web/
COPY --from=web /src/web/dist/ web/dist/
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/orglane ./core/cmd/orglane

# Docker CLI and Compose plugin for the agent image: the static release
# binaries for TARGETARCH. Docker publishes no checksums for its static
# archives, so the SHA-256 values were taken from the downloaded files; the
# Compose values match the .sha256 files of its GitHub release. To bump:
# change the versions and every checksum together.
FROM --platform=$BUILDPLATFORM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS docker-tools
ARG TARGETARCH
RUN set -eu; \
    docker_version=29.9.3; \
    compose_version=5.6.1; \
    case "$TARGETARCH" in \
    amd64) \
        arch=x86_64; \
        docker_sha256=3fb5d1f13014c18cc54945bc177c58a9056a482dada031e17d12cf2e6922c973; \
        compose_sha256=47737f4236e6d3ccea7291995203f8f018058783b4c3a9469119811a0b5db8c0 ;; \
    arm64) \
        arch=aarch64; \
        docker_sha256=1b27fd739378ef938401e81ce5afb73b7ad7432d4f598814a9ff5f826bb464d2; \
        compose_sha256=42cfa75f51ad0ccf290bad37899e0038a8c10a0069e85233ecc1d53e3ff1347d ;; \
    *) echo "no Docker tools pinned for $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    wget -q -O /tmp/docker.tgz "https://download.docker.com/linux/static/stable/$arch/docker-$docker_version.tgz"; \
    echo "$docker_sha256  /tmp/docker.tgz" | sha256sum -c -; \
    wget -q -O /tmp/docker-compose "https://github.com/docker/compose/releases/download/v$compose_version/docker-compose-linux-$arch"; \
    echo "$compose_sha256  /tmp/docker-compose" | sha256sum -c -; \
    mkdir -p /out; \
    tar -xzf /tmp/docker.tgz -C /tmp docker/docker; \
    install -m 0755 /tmp/docker/docker /out/docker; \
    install -m 0755 /tmp/docker-compose /out/docker-compose; \
    rm -rf /tmp/docker /tmp/docker.tgz /tmp/docker-compose

# Agent image (ADR 0019): `orglane agent`, the only component with Docker
# access. Same distroless base as the core; it runs as root because it talks
# to the Docker socket, which is root-equivalent anyway (the bundle says so
# with user: "0:0"). Built only with --target agent.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS agent
ARG VERSION=dev
LABEL org.opencontainers.image.title="orglane-agent" \
      org.opencontainers.image.description="Orglane agent: installs and runs managed services with Docker Compose" \
      org.opencontainers.image.source="https://github.com/orglane/orglane" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/orglane /usr/local/bin/orglane
COPY --from=docker-tools /out/docker /usr/local/bin/docker
# A system plugin directory: the CLI finds it whatever DOCKER_CONFIG says.
COPY --from=docker-tools /out/docker-compose /usr/local/libexec/docker/cli-plugins/docker-compose
USER 0:0
HEALTHCHECK --interval=10s --timeout=12s --start-period=30s --retries=3 \
    CMD ["/usr/local/bin/orglane", "agent", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/orglane"]
CMD ["agent"]

# Core image: the default target, so it must stay the last stage.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329 AS core
ARG VERSION=dev
LABEL org.opencontainers.image.title="orglane" \
      org.opencontainers.image.description="Orglane core: server, agent and cli" \
      org.opencontainers.image.source="https://github.com/orglane/orglane" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/orglane /usr/local/bin/orglane
USER nonroot:nonroot
# 8080: public listener (behind Traefik). The internal listener for Traefik's
# HTTP provider (server.internal_listen) is never published.
EXPOSE 8080
HEALTHCHECK --interval=10s --timeout=6s --start-period=60s --retries=3 \
    CMD ["/usr/local/bin/orglane", "cli", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/orglane"]
CMD ["server"]
