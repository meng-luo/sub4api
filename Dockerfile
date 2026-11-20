# syntax=docker/dockerfile:1.7
# =============================================================================
# Sub2API Multi-Stage Dockerfile
# =============================================================================
# Stage 1: Build frontend
# Stage 2: Build Go backend with embedded frontend
# Stage 3: Final minimal image
# =============================================================================

ARG NODE_IMAGE=node:24-alpine
ARG GOLANG_IMAGE=golang:1.27.4-alpine
ARG ALPINE_IMAGE=alpine:3.21
ARG POSTGRES_IMAGE=postgres:18-alpine
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ARG NPM_CONFIG_REGISTRY=

# -----------------------------------------------------------------------------
# Stage 1: Frontend Builder
# -----------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: the frontend output is JS (arch-neutral), so build
# it on the native host arch instead of under QEMU emulation for the target.
FROM --platform=${BUILDPLATFORM} ${NODE_IMAGE} AS frontend-builder
ARG NPM_CONFIG_REGISTRY

WORKDIR /app/frontend

# Install pnpm (pinned to v9 to match CI and keep builds reproducible)
RUN corepack enable && corepack prepare pnpm@9 --activate

# Install dependencies first (better caching)
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN --mount=type=cache,id=sub2api-pnpm-store,target=/root/.local/share/pnpm/store \
    if [ -n "${NPM_CONFIG_REGISTRY}" ]; then pnpm config set registry "${NPM_CONFIG_REGISTRY}"; fi && \
    pnpm install --frozen-lockfile --prefer-offline

# Copy frontend source and build.
# LegalDocumentView.vue (admin-compliance gate) build-time imports
# ../../../../docs/legal/*.md?raw, so docs/legal/ must sit beside frontend/
# in the image (WORKDIR /app/frontend -> resolves to /app/docs/legal/*.md).
# Copy only that subtree to keep the build dependency minimal.
COPY frontend/ ./
COPY docs/legal/ /app/docs/legal/
RUN pnpm run build

# -----------------------------------------------------------------------------
# Stage 2: Backend Builder
# -----------------------------------------------------------------------------
# --platform=$BUILDPLATFORM: run the Go toolchain on the native host arch and
# cross-compile to the target arch below. The binary is CGO_ENABLED=0, so this
# is a clean pure-Go cross-compile — no QEMU emulation of go mod download / go
# build (emulated networking here was dropping module fetches with EOF).
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS backend-builder

# Build arguments for version info (set by CI)
ARG VERSION=
ARG COMMIT=docker
ARG DATE
ARG GOPROXY
ARG GOSUMDB
# Populated by buildx from the --platform target (e.g. linux/amd64).
ARG TARGETOS
ARG TARGETARCH

ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}

# Install build dependencies
# bash/python3/zip/py3-cryptography 供 pluginimpl 的打包脚本生成 .s2plugin
# （清单哈希 + 可选 Ed25519 签名）。仅构建阶段需要，不进最终镜像。
RUN apk add --no-cache git ca-certificates tzdata bash python3 zip py3-cryptography

WORKDIR /app/backend

# Copy go mod files first (better caching)
COPY backend/go.mod backend/go.sum ./
# Cache mount keeps the module cache across builds so a transient CDN blip on
# retry resumes instead of re-fetching every zip from scratch.
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    go mod download

# Copy backend source first
COPY backend/ ./

# Copy frontend dist from previous stage (must be after backend copy to avoid being overwritten)
COPY --from=frontend-builder /app/backend/internal/web/dist ./internal/web/dist

# Build the binary (BuildType=release for CI builds, embed frontend)
# Version precedence: build arg VERSION > exact git tag > cmd/server/VERSION
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    VERSION_VALUE="${VERSION}" && \
    if [ -z "${VERSION_VALUE}" ]; then VERSION_VALUE="$(./scripts/resolve-version.sh)"; fi && \
    DATE_VALUE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -tags embed \
    -ldflags="-s -w -X main.Version=${VERSION_VALUE} -X main.Commit=${COMMIT} -X main.Date=${DATE_VALUE} -X main.BuildType=release" \
    -trimpath \
    -o /app/sub2api \
    ./cmd/server

# Build the self-hosted OpenAI transport plugin package (.s2plugin) for the same
# target arch, so the image ships an installable artifact instead of requiring a
# manual build on the deploy host.
# 签名是可选步骤：--build-arg PLUGIN_SIGN=1 且 --secret id=plugin_signing_key,src=<ed25519 私钥>
# 时产出可直接在 plugins.trusted_publishers 下安装的包；默认（PLUGIN_SIGN=0）为未签名包，
# 只能配合 plugins.allow_unsigned 在调试环境安装。
# sign.py 依赖 python-cryptography（构建阶段已装，最终镜像不含 python）。
#
# PLUGIN_SIGN 必须显式置 1 才会签名：BuildKit 的 secret 内容不参与层缓存键，
# 只挂 secret 而重建时可能命中「未签名」的旧层而静默给出未签名包。把开关写进
# 命令行让缓存键随签名意图变化；置 1 却拿不到私钥时直接失败，不静默降级。
ARG PLUGIN_VERSION=0.1.0
ARG PLUGIN_SIGN=0
RUN --mount=type=secret,id=plugin_signing_key,required=false \
    --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    if [ "${PLUGIN_SIGN}" = "1" ]; then \
        if [ ! -s /run/secrets/plugin_signing_key ]; then \
            echo "PLUGIN_SIGN=1 但未提供 build secret plugin_signing_key" >&2; exit 1; \
        fi; \
        export SIGN_KEY=/run/secrets/plugin_signing_key; \
    fi; \
    VERSION="${PLUGIN_VERSION}" OUT=/app/plugin-dist \
    bash ./pluginimpl/openaitransport/package/build.sh "linux-${TARGETARCH:-$(go env GOARCH)}"

# -----------------------------------------------------------------------------
# Stage 3: PostgreSQL Client (version-matched with docker-compose)
# -----------------------------------------------------------------------------
FROM ${POSTGRES_IMAGE} AS pg-client

# -----------------------------------------------------------------------------
# Stage 4: Final Runtime Image
# -----------------------------------------------------------------------------
FROM ${ALPINE_IMAGE}

# Labels
LABEL maintainer="Wei-Shaw <github.com/Wei-Shaw>"
LABEL description="Sub2API - AI API Gateway Platform"
LABEL org.opencontainers.image.source="https://github.com/MACOS-DO/sub4api"

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    su-exec \
    libpq \
    zstd-libs \
    lz4-libs \
    krb5-libs \
    libldap \
    libedit \
    && rm -rf /var/cache/apk/*

# Copy pg_dump and psql from the same postgres image used in docker-compose
# This ensures version consistency between backup tools and the database server
COPY --from=pg-client /usr/local/bin/pg_dump /usr/local/bin/pg_dump
COPY --from=pg-client /usr/local/bin/psql /usr/local/bin/psql
COPY --from=pg-client /usr/local/lib/libpq.so.5* /usr/local/lib/

# Create non-root user
RUN addgroup -g 1000 sub2api && \
    adduser -u 1000 -G sub2api -s /bin/sh -D sub2api

# Set working directory
WORKDIR /app

# Copy binary/resources with ownership to avoid extra full-layer chown copy
COPY --from=backend-builder --chown=sub2api:sub2api /app/sub2api /app/sub2api
COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources

# 插件包产物（.s2plugin + 解包目录）。安装：管理页 → 插件 → 上传，
# 或用 docker cp 从容器取出。签名包需 --build-arg PLUGIN_SIGN=1 + build secret。
COPY --from=backend-builder --chown=sub2api:sub2api /app/plugin-dist/ /app/plugin-packages/

# Create data directory
RUN mkdir -p /app/data && chown sub2api:sub2api /app /app/data

# Copy entrypoint script (fixes volume permissions then drops to sub2api)
COPY deploy/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh

# Expose port (can be overridden by SERVER_PORT env var)
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD wget -q -T 5 -O /dev/null http://localhost:${SERVER_PORT:-8080}/health || exit 1

# Run the application (entrypoint fixes /app/data ownership then execs as sub2api)
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/sub2api"]
