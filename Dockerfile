# Stage 1: build
FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/memento-knowledge/mkonnect/internal/version.Version=${VERSION}" -o /mkonnect ./cmd/mkonnect

# Empty directory copied into the runtime image below with the non-root uid. Docker
# initializes a freshly created (empty) named volume from the image's directory at the
# mount point, including its ownership — so mounting a new volume at /data yields a
# directory the runtime user (uid 65532) can write, without any host-side chown.
RUN mkdir /data-empty

# Stage 2: runtime (distroless, non-root)
FROM gcr.io/distroless/static:nonroot

ARG VERSION=dev
ARG REVISION=unknown

LABEL org.opencontainers.image.title="mkonnect" \
      org.opencontainers.image.description="Memento on-prem connector — bridges customer-internal tools to the Memento platform" \
      org.opencontainers.image.source="https://github.com/memento-knowledge/mkonnect" \
      org.opencontainers.image.licenses="MIT" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.revision="$REVISION"

# Ship /data owned by the non-root runtime user (uid 65532) so a mounted empty named
# volume inherits writable ownership and the connector can persist its key there.
COPY --from=builder --chown=65532:65532 /data-empty /data

COPY --from=builder /mkonnect /mkonnect

ENTRYPOINT ["/mkonnect"]
