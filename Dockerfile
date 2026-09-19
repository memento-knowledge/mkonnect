# Stage 1: build
FROM golang:1.25-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/memento-knowledge/mkonnect/internal/version.Version=${VERSION}" -o /mkonnect ./cmd/mkonnect

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

COPY --from=builder /mkonnect /mkonnect

ENTRYPOINT ["/mkonnect"]
