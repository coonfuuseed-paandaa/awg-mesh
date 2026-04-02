# ADR-0003: Traefik Integration — Hybrid Pattern

## Status

Accepted

## Context

All awg-mesh nodes run behind Traefik reverse proxy on the user's infrastructure. The question: should AWG UDP traffic go through Traefik, or bypass it?

awg-mesh-node exposes three ports:
- **51820/udp** — AmneziaWG data plane (peer tunnels)
- **9090/tcp** — gRPC management (mTLS + token auth)
- **9091/tcp** — Prometheus metrics (HTTP)

## Decision Drivers

* User wants to continue using Traefik for all services
* UDP traffic must not have unacceptable overhead or breakage
* gRPC with mTLS must work (TLS termination at the node, not Traefik)
* Solution must work with Docker Compose labels

## Research Findings (Oracle, verified)

### UDP through Traefik breaks WireGuard

Traefik UDP proxy replaces the source IP with Traefik's container IP. WireGuard uses source IP+port for:
- Peer identification and routing table construction
- Handshake association
- Reply address tracking

When all peers appear from Traefik's IP, WG cannot distinguish them. Only one peer can have an active session. ProxyProtocol has no UDP equivalent — there is no way to pass the real source IP.

Community confirms: wg-easy official Traefik guide binds WG UDP directly, bypassing Traefik entirely.

Sources: community.traefik.io/t/wireguard-behind-traefik/26473, mintlify.com/wg-easy deployment guide.

### gRPC with mTLS passthrough — works perfectly

Traefik TCP router with `tls.passthrough=true` forwards the raw TLS stream without decryption. mTLS handshake happens at awg-mesh-node. Traefik sees only opaque bytes after SNI peek.

### Prometheus HTTP — standard Traefik routing

No issues. Standard HTTP router with Host rule.

## Decision

**Hybrid pattern: Traefik for TCP/HTTP, direct port for UDP.**

| Port | Protocol | Via Traefik | Why |
|------|----------|-------------|-----|
| 51820 | UDP | **NO** — direct `ports:` binding | Source IP masquerading breaks WG peer identification |
| 9090 | TCP | **YES** — TCP router, TLS passthrough | mTLS at node, Traefik just forwards stream |
| 9091 | TCP/HTTP | **YES** — HTTP router | Standard metrics endpoint |

## Consequences

### Positive

- WireGuard works correctly with proper peer identification
- gRPC benefits from Traefik's TCP routing (HostSNI, health checks)
- Prometheus metrics accessible via Traefik HTTP routing
- No UDP performance overhead (zero extra kernel/userspace crossings)
- Follows established community pattern (wg-easy, other VPN containers)

### Negative

- AWG port (51820) must be exposed directly in Docker Compose `ports:` section
- Two routing paths: Traefik for management, direct for data plane
- Traefik dashboard won't show AWG traffic stats

### Configuration

Docker Compose with Traefik labels:

```yaml
services:
  awg-mesh-node:
    image: ghcr.io/coonfuuseed-paandaa/awg-mesh-node:latest
    restart: unless-stopped
    cap_add:
      - NET_ADMIN
      - NET_RAW
    devices:
      - /dev/net/tun:/dev/net/tun
    volumes:
      - /srv/awg-mesh:/config
    ports:
      # AWG data plane — DIRECT, bypasses Traefik
      - "51820:51820/udp"
    labels:
      # gRPC management — via Traefik TCP with mTLS passthrough
      - "traefik.enable=true"
      - "traefik.tcp.routers.awg-grpc.entrypoints=awg-grpc"
      - "traefik.tcp.routers.awg-grpc.rule=HostSNI(`*`)"
      - "traefik.tcp.routers.awg-grpc.tls.passthrough=true"
      - "traefik.tcp.routers.awg-grpc.service=awg-grpc-svc"
      - "traefik.tcp.services.awg-grpc-svc.loadbalancer.server.port=9090"
      # Prometheus metrics — via Traefik HTTP
      - "traefik.http.routers.awg-metrics.rule=Host(`node.example.com`) && PathPrefix(`/metrics`)"
      - "traefik.http.routers.awg-metrics.entrypoints=web"
      - "traefik.http.routers.awg-metrics.service=awg-metrics-svc"
      - "traefik.http.services.awg-metrics-svc.loadbalancer.server.port=9091"
    command:
      - --mode=master
      - --name=master-01
      - --topology=/config/mesh-topology.yml
```

Traefik static config addition:

```yaml
entryPoints:
  awg-grpc:
    address: ":9090"
```

## Related Decisions

- ADR-0001: Multi-Image Docker Strategy — same ports, different images
- Constitution C5: mTLS mandatory — TLS passthrough preserves this

## References

- Traefik docs: TCP TLS passthrough, UDP entrypoints
- wg-easy Traefik deployment: WG UDP bypasses Traefik
- community.traefik.io: WireGuard source IP masquerading issue
