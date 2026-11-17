# syntax=docker/dockerfile:1

# Base images are pinned by digest; Dependabot keeps tags and digests current.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -buildvcs=false \
      -ldflags="-s -w -X main.version=${VERSION}" \
      -o /out/k8s-app-standby-toggle ./cmd/k8s-app-standby-toggle

# Distroless "static": no shell, no package manager, only CA certificates,
# tzdata and /etc/passwd. Runs as the unprivileged "nonroot" user (65532).
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG VERSION=dev
LABEL org.opencontainers.image.title="k8s-app-standby-toggle" \
      org.opencontainers.image.description="Puts groups of Argo CD Applications to sleep (scaled to zero) and wakes them again" \
      org.opencontainers.image.source="https://github.com/brnl/k8s-app-standby-toggle" \
      org.opencontainers.image.version="${VERSION}"
COPY --from=build /out/k8s-app-standby-toggle /k8s-app-standby-toggle
USER 65532:65532
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=15s --retries=3 \
  CMD ["/k8s-app-standby-toggle", "healthcheck"]
ENTRYPOINT ["/k8s-app-standby-toggle"]
