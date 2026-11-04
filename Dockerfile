# syntax=docker/dockerfile:1
#
# Orglane core image: a static binary with the web UI embedded, on distroless,
# running as non-root. Build: make image (or docker build -t orglane:dev .).
# Releases build it for linux/amd64 and linux/arm64 (.github/workflows/
# images.yml): the web and Go stages run on the build platform, Go
# cross-compiles for TARGETPLATFORM, and the final stage has no RUN step.
#
# Base images are pinned by version and digest. Bump both together; the Go
# and Node.js versions follow go.mod and .nvmrc.

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
COPY providers/ providers/
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

FROM gcr.io/distroless/static-debian13:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
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
