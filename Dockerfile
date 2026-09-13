# Target registry: 758890152938.dkr.ecr.us-east-1.amazonaws.com/memento-connector
# Build: docker build -t 758890152938.dkr.ecr.us-east-1.amazonaws.com/memento-connector:latest .
# Do NOT push automatically; authenticate with ECR first.

# Stage 1: build
FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /mkonnect ./cmd/mkonnect

# Stage 2: runtime (distroless, non-root)
FROM gcr.io/distroless/static:nonroot

LABEL org.opencontainers.image.title="mkonnect" \
      org.opencontainers.image.description="Memento on-prem connector — bridges customer-internal tools to the Memento platform" \
      org.opencontainers.image.source="https://github.com/memento-knowledge/mkonnect" \
      org.opencontainers.image.licenses="Proprietary"

COPY --from=builder /mkonnect /mkonnect

ENTRYPOINT ["/mkonnect"]
