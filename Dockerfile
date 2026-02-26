# Build stage
FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)" -o /truenas-net-exporter .

# Runtime stage — minimal image.
FROM debian:bookworm-slim

RUN apt-get update && \
    apt-get install -y --no-install-recommends ca-certificates && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /truenas-net-exporter /usr/local/bin/truenas-net-exporter

EXPOSE 9551

ENTRYPOINT ["truenas-net-exporter"]
CMD ["--path.rootfs=/host", "--path.procfs=/host/proc", "--docker.socket=/host/var/run/docker.sock"]
