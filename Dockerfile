# syntax=docker/dockerfile:1
#
# Image bundling three binaries:
#   /usr/local/bin/victoria-logs-prod             VictoriaLogs (from the official image)
#   /usr/local/bin/victoria_logs_s3_backup        backup tool (this project)
#   /usr/local/bin/victoria_logs_s3_restore_svr   restore server (this project)
#
# The project binaries are compiled from source in the `builder` stage. The
# builder runs on the build host's native platform (BUILDPLATFORM) and
# cross-compiles for TARGETOS/TARGETARCH (CGO disabled), so multi-arch builds
# don't need QEMU for the Go compile step.
#
#   docker build -t victoria-logs-s3-adapter .
#
#   # multi-arch
#   docker buildx build --platform linux/amd64,linux/arm64 -t <repo>:<tag> --push .
#
#   # override versions
#   docker build --build-arg GO_VERSION=1.27 --build-arg VICTORIA_LOGS_VERSION=v1.50.0 --build-arg ALPINE_VERSION=3.23 .
#
# No ENTRYPOINT is set; pick the binary per container, e.g.
#   victoria_logs_s3_restore_svr -victoria.logs.bin=/usr/local/bin/victoria-logs-prod ...
#   victoria_logs_s3_backup -backup.yesterday.at=0015 ...
#   victoria-logs-prod -storageDataPath=/data ...

ARG GO_VERSION=1.27
ARG ALPINE_VERSION=3.23
ARG VICTORIA_LOGS_VERSION=v1.50.0

FROM victoriametrics/victoria-logs:${VICTORIA_LOGS_VERSION} AS victoria-logs

FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-alpine AS builder

ARG TARGETOS
ARG TARGETARCH

WORKDIR /src

# Download modules first so this layer is cached until go.mod/go.sum change.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY cmd/ cmd/
COPY internal/ internal/

# Same flags as the Makefile: CGO_ENABLED=0, -trimpath, -ldflags '-s -w'.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags '-s -w' -o /out/ ./cmd/...

FROM alpine:${ALPINE_VERSION}

COPY --from=victoria-logs /victoria-logs-prod /usr/local/bin/victoria-logs-prod
COPY --from=builder /out/victoria_logs_s3_backup \
     /out/victoria_logs_s3_restore_svr \
     /usr/local/bin/

# 8093: victoria_logs_s3_restore_svr (-addr), 9428: victoria-logs-prod (-httpListenAddr)
EXPOSE 8093 9428
