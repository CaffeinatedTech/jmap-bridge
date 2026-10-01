# syntax=docker/dockerfile:1
# jmap-bridge image (FR-D.1, NFR-10): a fully static, non-root binary on
# a distroless base, with /config and /data volumes and one entrypoint.
# CGO is off so the binary carries no libc requirement and the same build
# works for amd64 and arm64 (buildx fills TARGETOS/TARGETARCH).

# ---- build -----------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build \
    -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/jmap-bridge ./cmd/jmap-bridge

# ---- runtime ---------------------------------------------------------------
FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
ARG REVISION=unknown
LABEL org.opencontainers.image.title="jmap-bridge" \
      org.opencontainers.image.description="A JMAP server that fronts IMAP/SMTP accounts and their CardDAV contacts." \
      org.opencontainers.image.source="https://github.com/CaffeinatedTech/jmap-bridge" \
      org.opencontainers.image.url="https://github.com/CaffeinatedTech/jmap-bridge" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${REVISION}"

COPY --from=build /out/jmap-bridge /usr/local/bin/jmap-bridge
COPY LICENSE /licenses/LICENSE
# The distroless nonroot tag already runs as uid/gid 65532; make it
# explicit so a re-tag cannot silently change it.
USER nonroot:nonroot
EXPOSE 8080
VOLUME ["/config", "/data"]
ENTRYPOINT ["/usr/local/bin/jmap-bridge"]
CMD ["--config", "/config/config.toml"]
