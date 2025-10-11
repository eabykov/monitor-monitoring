FROM golang:1.25-alpine AS builder

WORKDIR /build

RUN apk add --no-cache git ca-certificates tzdata

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-s -w -X main.version=$(date +%Y%m%d-%H%M%S)" \
    -o service-monitor \
    main.go

FROM alpine:3.22

RUN apk --no-cache add ca-certificates tzdata && \
    addgroup -g 1000 monitor && \
    adduser -D -u 1000 -G monitor monitor

WORKDIR /app

COPY --from=builder /build/service-monitor .
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY config.yaml ./

RUN chown -R monitor:monitor /app

USER monitor

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ps aux | grep service-monitor | grep -v grep || exit 1

ENV TZ=UTC

ENTRYPOINT ["./service-monitor"]
