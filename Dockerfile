# syntax=docker/dockerfile:1
# M0 image (FR-D.1): static non-root binary, /config and /data volumes,
# one entrypoint. CGO is off so the binary is fully static (NFR-10).

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/jmap-bridge ./cmd/jmap-bridge

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/jmap-bridge /usr/local/bin/jmap-bridge
EXPOSE 8080
VOLUME ["/config", "/data"]
ENTRYPOINT ["/usr/local/bin/jmap-bridge"]
CMD ["--config", "/config/config.toml"]
