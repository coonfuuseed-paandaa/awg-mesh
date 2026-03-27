FROM golang:1.25-alpine AS builder

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-X main.version=${VERSION} -s -w" -o /build/bin/awg-mesh-node ./cmd/awg-mesh-node

FROM alpine:3.21

RUN apk add --no-cache iptables ip6tables iproute2

COPY --from=builder /build/bin/awg-mesh-node /usr/local/bin/awg-mesh-node

RUN mkdir -p /config

VOLUME ["/config"]

EXPOSE 9090 9091

# Runs as root: required for iptables, ip route, TUN device management.
# Use --cap-add=NET_ADMIN --cap-add=NET_RAW in docker-compose.
ENTRYPOINT ["/usr/local/bin/awg-mesh-node"]
