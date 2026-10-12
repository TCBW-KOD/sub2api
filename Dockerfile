# syntax=docker/dockerfile:1.7
# Sub2API CPR development build. Upstream license and author notices retained.
# The audited integration step fails closed if upstream source has changed.

ARG NODE_IMAGE=node:24-alpine
ARG GOLANG_IMAGE=golang:1.27.2-alpine
ARG ALPINE_IMAGE=alpine:3.21
ARG POSTGRES_IMAGE=postgres:18-alpine
ARG GOPROXY=https://goproxy.cn,direct
ARG GOSUMDB=sum.golang.google.cn
ARG NPM_CONFIG_REGISTRY=

# Materialize the small, hash-checked integration hooks for both build stages.
# This stage never uses credentials, publishes code or contacts a model upstream.
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS cpr-source
RUN apk add --no-cache python3
WORKDIR /src
COPY backend/ ./backend/
COPY frontend/ ./frontend/
COPY scripts/cpr-native/prepare.py ./scripts/cpr-native/prepare.py
RUN python3 scripts/cpr-native/prepare.py --repo /src && \
    gofmt -w backend/internal/pkg/cprnative \
    backend/internal/service/cpr_native*.go backend/internal/handler/cpr_native_handler.go

# Architecture-neutral frontend build.
FROM --platform=${BUILDPLATFORM} ${NODE_IMAGE} AS frontend-builder
ARG NPM_CONFIG_REGISTRY
WORKDIR /app/frontend
RUN corepack enable && corepack prepare pnpm@9 --activate
COPY frontend/package.json frontend/pnpm-lock.yaml ./
RUN --mount=type=cache,id=sub2api-pnpm-store,target=/root/.local/share/pnpm/store \
    if [ -n "${NPM_CONFIG_REGISTRY}" ]; then pnpm config set registry "${NPM_CONFIG_REGISTRY}"; fi && \
    pnpm install --frozen-lockfile --prefer-offline
COPY --from=cpr-source /src/frontend/ ./
COPY docs/legal/ /app/docs/legal/
RUN pnpm run build

# Native build host with Go cross compilation for the final runtime architecture.
FROM --platform=${BUILDPLATFORM} ${GOLANG_IMAGE} AS backend-builder
ARG VERSION=0.2.15-cpr-native-preview
ARG COMMIT=docker
ARG DATE
ARG GOPROXY
ARG GOSUMDB
ARG TARGETOS
ARG TARGETARCH
ENV GOPROXY=${GOPROXY}
ENV GOSUMDB=${GOSUMDB}
RUN apk add --no-cache git ca-certificates tzdata
WORKDIR /app/backend
COPY backend/go.mod backend/go.sum ./
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod go mod download
COPY --from=cpr-source /src/backend/ ./
COPY --from=frontend-builder /app/backend/internal/web/dist ./internal/web/dist
RUN --mount=type=cache,id=sub2api-gomod,target=/go/pkg/mod \
    --mount=type=cache,id=sub2api-gobuild,target=/root/.cache/go-build \
    VERSION_VALUE="${VERSION}" && \
    if [ -z "${VERSION_VALUE}" ]; then VERSION_VALUE="$(./scripts/resolve-version.sh)"; fi && \
    DATE_VALUE="${DATE:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}" && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -tags embed \
    -ldflags="-s -w -X main.Version=${VERSION_VALUE} -X main.Commit=${COMMIT} -X main.Date=${DATE_VALUE} -X main.BuildType=release" \
    -trimpath -o /app/sub2api ./cmd/server

FROM ${POSTGRES_IMAGE} AS pg-client
FROM ${ALPINE_IMAGE}
LABEL maintainer="Wei-Shaw <github.com/Wei-Shaw>"
LABEL description="Sub2API - CPR native upstream development preview"
LABEL org.opencontainers.image.source="https://github.com/TCBW-KOD/sub2api"
RUN apk add --no-cache \
    ca-certificates tzdata su-exec libpq zstd-libs lz4-libs krb5-libs libldap libedit \
    && rm -rf /var/cache/apk/*
COPY --from=pg-client /usr/local/bin/pg_dump /usr/local/bin/pg_dump
COPY --from=pg-client /usr/local/bin/psql /usr/local/bin/psql
COPY --from=pg-client /usr/local/lib/libpq.so.5* /usr/local/lib/
RUN addgroup -g 1000 sub2api && \
    adduser -u 1000 -G sub2api -s /bin/sh -D sub2api
WORKDIR /app
COPY --from=backend-builder --chown=sub2api:sub2api /app/sub2api /app/sub2api
COPY --from=backend-builder --chown=sub2api:sub2api /app/backend/resources /app/resources
RUN mkdir -p /app/data && chown sub2api:sub2api /app/data
COPY deploy/docker-entrypoint.sh /app/docker-entrypoint.sh
RUN chmod +x /app/docker-entrypoint.sh
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD wget -q -T 5 -O /dev/null http://localhost:${SERVER_PORT:-8080}/health || exit 1
ENTRYPOINT ["/app/docker-entrypoint.sh"]
CMD ["/app/sub2api"]
