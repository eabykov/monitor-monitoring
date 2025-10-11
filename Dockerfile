# Build stage
FROM golang:1.25-alpine AS builder

WORKDIR /build

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY main.go ./

# Build binary with optimizations
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(date +%Y%m%d-%H%M%S)" \
    -o service-monitor \
    main.go

# Final stage
FROM alpine:latest

# Install runtime dependencies
RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -g 1000 monitor && \
    adduser -D -u 1000 -G monitor monitor

WORKDIR /app

# Copy binary and certificates from builder
COPY --from=builder /build/service-monitor .
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy config (optional, can be mounted as volume)
COPY config.yaml ./

# Change ownership
RUN chown -R monitor:monitor /app

# Switch to non-root user
USER monitor

# Health check
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ps aux | grep service-monitor | grep -v grep || exit 1

# Set timezone (default UTC)
ENV TZ=UTC

ENTRYPOINT ["./service-monitor"]
