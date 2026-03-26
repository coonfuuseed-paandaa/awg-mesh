# AWG Mesh: Unified Node Design

**Part of:** mesh-router-design.md
**Created:** 2026-03-26

## Decision: Single Container Per Host

All functionality merged into one binary (`awg-mesh-node`), one Docker image.

### Eliminated by this decision

- mesh-router as separate container
- Docker socket mount
- Docker label discovery
- Multiple containers per master (was N+2, now 1)
- Container orchestration via SSH/gRPC
- Docker API dependency

### awg-mesh-node modes

```
--mode master    : ingress + N egress tunnels + routing + healthcheck + capture
--mode endpoint  : AWG server + NAT + overlay IP
--mode client    : AWG client tunnels (Linux Docker)
--mode thin      : config-only, no gRPC (MikroTik / constrained environments)
```

### Master Node Internals

```
awg-mesh-node --mode master
├── gRPC API (:9090, mTLS)
│   ├── AddTunnel / RemoveTunnel
│   ├── RotateParams
│   ├── CaptureRefresh
│   ├── GetStatus / HealthCheck
│   └── Init (token auth, one-time)
├── AWG Interfaces (amneziawg-go userspace)
│   ├── wg-ingress  — server for MikroTik/Linux clients
│   ├── wg-kz-01    — tunnel to kz-01 endpoint
│   ├── wg-kz-02    — tunnel to kz-02 endpoint
│   ├── wg-kz-03    — tunnel to kz-03 endpoint
│   ├── wg-pl-01    — tunnel to pl-01 endpoint
│   └── wg-us-01    — tunnel to us-01 endpoint
├── Router
│   ├── Per-node routes: 172.20.70.21/32 → wg-kz-01
│   ├── Balancer ECMP: 172.20.70.20 → nexthop wg-kz-01,02,03
│   └── Overlay route on ingress: 172.20.70.0/24 via self
├── Healthcheck (per tunnel)
│   ├── Ping overlay IP every 10s
│   ├── 3 failures → remove from routing + balancer
│   └── Recovery → re-add
├── Capture Engine
│   ├── TLS/QUIC packet capture from domain list
│   ├── Stored in /config/capture-data/
│   └── Daily cron refresh
└── Rotation Scheduler
    ├── Tier 1: J/I params per configurable interval
    └── Triggers via gRPC or internal schedule
```

### Endpoint Node Internals

```
awg-mesh-node --mode endpoint
├── gRPC API (:9090, mTLS)
│   ├── AddPeer / RemovePeer
│   ├── RotateParams
│   ├── GetStatus / HealthCheck
│   └── Init
├── AWG Interface
│   └── wg0 — server, accepts peers from masters
├── NAT
│   └── iptables MASQUERADE for egress traffic
└── Overlay IP
    └── 172.20.70.XX/32 on loopback
```

### Client Node Internals (Linux)

```
awg-mesh-node --mode client
├── gRPC API (:9090, mTLS)
├── AWG Interfaces
│   ├── wg-master-01 — tunnel to master-01
│   └── wg-master-02 — tunnel to master-02
└── Routing
    └── 172.20.70.0/24 via tunnels (ECMP if multiple masters)
```

### Thin Client (MikroTik)

```
awg-mesh-node --mode thin
├── Single AWG interface (wg0)
├── Config file: /config/wg0.conf
├── No gRPC, no API
└── Rotation: config file replaced + process restart
```

## Docker Compose Templates

### Master

```yaml
services:
  awg-mesh:
    image: ghcr.io/thebtf/awg-mesh-node:latest
    container_name: awg-mesh
    command: ["--mode", "master"]
    restart: always
    cap_add: [NET_ADMIN]
    devices: [/dev/net/tun]
    ports:
      - "853:853/udp"      # AWG ingress for clients
      - "9090:9090"        # gRPC management
    volumes:
      - ${CONFIG_DIR:-/srv/awg-mesh}:/config
    environment:
      - MESH_TOKEN=${MESH_TOKEN}
    sysctls:
      - net.ipv4.ip_forward=1
```

### Endpoint

```yaml
services:
  awg-mesh:
    image: ghcr.io/thebtf/awg-mesh-node:latest
    container_name: awg-mesh
    command: ["--mode", "endpoint"]
    restart: always
    cap_add: [NET_ADMIN]
    devices: [/dev/net/tun]
    ports:
      - "853:853/udp"      # AWG server for masters
      - "9090:9090"        # gRPC management
    volumes:
      - ${CONFIG_DIR:-/srv/awg-mesh}:/config
    environment:
      - MESH_TOKEN=${MESH_TOKEN}
    sysctls:
      - net.ipv4.ip_forward=1
```

## mesh-ctl → awg-mesh-node Interaction

All management via gRPC. No SSH, no Docker API.

```
mesh-ctl endpoint init → gRPC Init() on endpoint
                       → gRPC AddTunnel() on each master

mesh-ctl rotate        → gRPC RotateParams() on endpoint + masters

mesh-ctl status        → gRPC GetStatus() on all nodes
```

Master internally manages its own tunnels. No external container orchestration.
