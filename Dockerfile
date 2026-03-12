FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /app

COPY src/go.mod src/go.sum ./

RUN go mod download

COPY src/ .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags='-w -s -extldflags "-static"' \
    -a -installsuffix cgo \
    -o server \
    ./cmd/server

FROM alpine:3.19

RUN apk --no-cache add \
    ca-certificates \
    curl \
    kubectl \
    && wget -O /usr/local/bin/virtctl \
    https://github.com/kubevirt/kubevirt/releases/download/v1.5.1/virtctl-v1.5.1-linux-amd64 \
    && chmod +x /usr/local/bin/virtctl

RUN addgroup -g 1001 -S appgroup && \
    adduser -u 1001 -S appuser -G appgroup

WORKDIR /app

COPY --from=builder /app/server .

COPY --from=builder /app/templates ./templates
COPY --from=builder /app/scenarios ./scenarios

RUN chown -R appuser:appgroup /app

USER appuser

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=10s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

CMD ["./server"]
