# AWG Mesh: Docker-native L3 Mesh Network

**Status:** Approved design
**Created:** 2026-03-26
**Language:** Go
**Components:** mesh-router (data plane) + mesh-ctl (control plane CLI)

## Problem

Current AWG relay routing on RU nodes uses dirty hacks:
- Source-based `ip rule` inside wg-easy container (per-peer, rigid)
- 6+ WG_POST_UP commands in one env var (fragile, unreadable)
- Separate Docker network per relay destination (doesn't scale)
- wg-easy connected to 3+ networks (complex, hard to debug)
- No failover, no health checks, no load balancing
- Adding an endpoint requires touching wg-easy config

## Solution

A lightweight Go service running as a Docker container that:
1. Watches Docker events for containers with `mesh.*` labels
2. Builds and maintains a Linux routing table based on labels
3. Provides health checking and failover
4. Single shared Docker network for all mesh participants

Like Traefik for HTTP routing, but for L3/IP routing.

## Architecture

```
┌──────────────────────────────────────────────────────┐
│ RU Relay Node (Docker host)                          │
│                                                      │
│  ┌─────────────┐  mesh network (172.20.80.0/24)     │
│  │ mesh-router  │◄──────────────────────────────┐    │
│  │ 172.20.80.1  │  routes built from labels     │    │
│  └──────┬───────┘                               │    │
│         │                                       │    │
│    ┌────┴────┐                                  │    │
│    │ wg-easy  │ ← MikroTik AWG tunnel           │    │
│    │ .80.2    │   WG_POST_UP: route 70.0/24     │    │
│    │          │   via 172.20.80.1                │    │
│    └──────────┘                                  │    │
│                                                  │    │
│  ┌───────────────┐ ┌───────────────┐ ┌────────┐ │    │
│  │wg-client-kz-01│ │wg-client-kz-02│ │wg-pl-01│ │    │
│  │ .80.21        │ │ .80.22        │ │ .80.31 │ │    │
│  │ mesh.overlay  │ │ mesh.overlay  │ │ mesh.. │ │    │
│  │ =172.20.70.21 │ │ =172.20.70.22 │ │ =70.31│ │    │
│  └───────┬───────┘ └───────┬───────┘ └────┬───┘ │    │
│          │ AWG              │ AWG          │ AWG  │    │
│          ▼                  ▼              ▼      │    │
│       kz-01              kz-02          pl-01    │    │
└──────────────────────────────────────────────────────┘
```

## Docker Compose (per master node)

```yaml
networks:
  default:
    driver: bridge
    ipam:
      config:
        - subnet: ${DOCKER_SUBNET}  # e.g. 172.20.41.0/24
  mesh:
    driver: bridge
    ipam:
      config:
        - subnet: 172.20.80.0/24

services:
  mesh-router:
    image: ghcr.io/thebtf/mesh-router:latest
    container_name: mesh-router
    restart: always
    networks:
      default:
      mesh:
        ipv4_address: 172.20.80.1
    cap_add:
      - NET_ADMIN
    sysctls:
      - net.ipv4.ip_forward=1
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      MESH_NETWORK: mesh             # Docker network name to discover peers
      MESH_LOG_LEVEL: info
      HEALTHCHECK_INTERVAL: 10s      # ping overlay IPs every 10s
      HEALTHCHECK_TIMEOUT: 3s
      HEALTHCHECK_FAILURES: 3        # remove route after 3 failures

  wg-easy:
    image: ghcr.io/wg-easy/wg-easy:15.2.2
    container_name: wg-easy
    networks:
      default:
      mesh:
        ipv4_address: 172.20.80.2
    environment:
      WG_POST_UP: "ip route add 172.20.70.0/24 via 172.20.80.1 || true"
      WG_POST_DOWN: "ip route del 172.20.70.0/24 via 172.20.80.1 || true"
      # ... other wg-easy config (ports, init, etc.)

  wg-client-kz-01:
    image: ghcr.io/thebtf/amneziawg-client:main
    container_name: wg-client-kz-01
    restart: always
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun
    sysctls:
      - net.ipv4.ip_forward=1
    volumes:
      - /srv/wg-client-kz-01:/config
    networks:
      mesh:
        ipv4_address: 172.20.80.21
    labels:
      - mesh.enable=true
      - mesh.overlay-ip=172.20.70.21
      - mesh.region=kz
      - mesh.node=kz-01

  wg-client-kz-02:
    image: ghcr.io/thebtf/amneziawg-client:main
    container_name: wg-client-kz-02
    restart: always
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun
    sysctls:
      - net.ipv4.ip_forward=1
    volumes:
      - /srv/wg-client-kz-02:/config
    networks:
      mesh:
        ipv4_address: 172.20.80.22
    labels:
      - mesh.enable=true
      - mesh.overlay-ip=172.20.70.22
      - mesh.region=kz
      - mesh.node=kz-02

  wg-client-pl-01:
    image: ghcr.io/thebtf/amneziawg-client:main
    container_name: wg-client-pl-01
    restart: always
    cap_add:
      - NET_ADMIN
    devices:
      - /dev/net/tun
    sysctls:
      - net.ipv4.ip_forward=1
    volumes:
      - /srv/wg-client-pl-01:/config
    networks:
      mesh:
        ipv4_address: 172.20.80.31
    labels:
      - mesh.enable=true
      - mesh.overlay-ip=172.20.70.31
      - mesh.region=pl
      - mesh.node=pl-01
```

## Labels API

| Label | Required | Type | Example | Purpose |
|-------|----------|------|---------|---------|
| `mesh.enable` | yes | bool | `true` | Register in mesh routing |
| `mesh.overlay-ip` | yes | IP | `172.20.70.21` | Overlay IP of remote node |
| `mesh.region` | no | string | `kz` | Region grouping |
| `mesh.node` | no | string | `kz-01` | Node identifier |
| `mesh.weight` | no | int | `100` | ECMP weight (default 100) |
| `mesh.backup` | no | bool | `true` | Backup route (higher metric) |
| `mesh.healthcheck` | no | string | `ping` | Health check type: ping, tcp, none |
| `mesh.healthcheck-target` | no | IP | `172.20.70.21` | What to ping (default: overlay-ip) |
| `mesh.nat` | no | bool | `true` | SNAT traffic exiting via this tunnel |

## mesh-router Go Service

### Core Loop

```
1. INIT
   - Enable ip_forward
   - Setup NAT (iptables MASQUERADE)
   - Scan existing containers with mesh.enable=true
   - Build initial routing table

2. WATCH
   - Docker events: container start, stop, die, destroy
   - On start: read labels → add route
   - On stop/die: remove route

3. HEALTHCHECK (goroutine per overlay-ip)
   - Ping overlay-ip every HEALTHCHECK_INTERVAL
   - After HEALTHCHECK_FAILURES consecutive failures → remove route
   - On recovery → re-add route
   - Log state transitions

4. API (optional, for debugging)
   - GET /routes — current routing table
   - GET /health — mesh-router health
   - GET /peers — discovered peers with status
```

### Route Management

```go
// On container start with mesh.enable=true:
func addRoute(overlayIP, meshIP string, backup bool) {
    metric := 100
    if backup { metric = 200 }
    exec("ip route add %s/32 via %s metric %d", overlayIP, meshIP, metric)
}

// On container stop:
func removeRoute(overlayIP string) {
    exec("ip route del %s/32", overlayIP)
}

// Health check failure:
func markUnhealthy(overlayIP string) {
    exec("ip route del %s/32", overlayIP)
    log("peer %s marked unhealthy", overlayIP)
}

// Health check recovery:
func markHealthy(overlayIP, meshIP string, backup bool) {
    addRoute(overlayIP, meshIP, backup)
    log("peer %s recovered", overlayIP)
}
```

### Docker Label Discovery

```go
func discoverPeers(cli *client.Client, networkName string) []Peer {
    containers, _ := cli.ContainerList(ctx, filters.NewArgs(
        filters.Arg("label", "mesh.enable=true"),
    ))

    peers := []Peer{}
    for _, c := range containers {
        overlayIP := c.Labels["mesh.overlay-ip"]
        meshIP := getContainerIPOnNetwork(c, networkName)
        region := c.Labels["mesh.region"]
        node := c.Labels["mesh.node"]
        backup := c.Labels["mesh.backup"] == "true"

        peers = append(peers, Peer{
            OverlayIP: overlayIP,
            MeshIP:    meshIP,
            Region:    region,
            Node:      node,
            Backup:    backup,
        })
    }
    return peers
}
```

## Traffic Flow (complete)

### Outbound (client → internet via VPN)

```
1. Client (192.168.x.x) → MikroTik
2. MikroTik DNS resolves youtube.com → IP in dynamic-youtube list
3. Mangle marks: routing-mark=vpn-route-kz
4. Route table vpn-route-kz: gateway=172.20.70.21
5. MikroTik reaches 172.20.70.0/24 via AWG tunnel to master
6. master wg-easy receives packet (dest: youtube IP, via 172.20.70.21)
7. wg-easy routes 172.20.70.0/24 → mesh-router (172.20.80.1)
8. mesh-router routes 172.20.70.21/32 → wg-client-kz-01 (172.20.80.21)
9. wg-client-kz-01 encapsulates in AWG tunnel → kz-01 endpoint
10. kz-01 NATs and sends to youtube.com
```

### Return (internet → client)

```
1. youtube.com responds to kz-01 public IP
2. kz-01 de-NATs → sends back through AWG tunnel to wg-client-kz-01
3. wg-client-kz-01 → mesh-router (reverse path)
4. mesh-router → wg-easy (return to MikroTik tunnel)
5. wg-easy → AWG tunnel → MikroTik
6. MikroTik → client
```

## Overlay Address Space

```
172.20.70.0/24 — mesh overlay (node identity)
  .1    = MikroTik gateway
  .11   = ru-01
  .12   = ru-02
  .21   = kz-01
  .22   = kz-02
  .23   = kz-03
  .31   = pl-01
  .41   = us-01

172.20.80.0/24 — mesh transport (Docker internal, per relay)
  .1    = mesh-router
  .2    = wg-easy (MikroTik termination)
  .21   = wg-client-kz-01
  .22   = wg-client-kz-02
  .23   = wg-client-kz-03
  .31   = wg-client-pl-01
  .41   = wg-client-us-01

Convention: mesh transport IP .8X.YZ mirrors overlay .70.YZ
```

## Adding a New Node (workflow)

### 1. Deploy AWG node (already automated)
```bash
# On new node (e.g., kz-04):
# Generic template + .env with OVERLAY_IP=172.20.70.24
ip addr add 172.20.70.24/32 dev lo
```

### 2. Create peer on endpoint node's wg-easy
```
# On kz-04 wg-easy: create peer for master
```

### 3. Add wg-client on master
```yaml
# Add to docker-compose.yml on master:
wg-client-kz-04:
  image: ghcr.io/thebtf/amneziawg-client:main
  container_name: wg-client-kz-04
  cap_add: [NET_ADMIN]
  devices: [/dev/net/tun]
  sysctls: [net.ipv4.ip_forward=1]
  volumes: [/srv/wg-client-kz-04:/config]
  networks:
    mesh:
      ipv4_address: 172.20.80.24
  labels:
    - mesh.enable=true
    - mesh.overlay-ip=172.20.70.24
    - mesh.region=kz
    - mesh.node=kz-04

# Then:
docker compose up -d wg-client-kz-04
```

### 4. mesh-router auto-discovers (no manual steps)
```
# mesh-router sees Docker event: container start
# Reads labels: mesh.overlay-ip=172.20.70.24, mesh network IP=172.20.80.24
# Adds route: ip route add 172.20.70.24/32 via 172.20.80.24
# Starts healthcheck ping to 172.20.70.24
# Done.
```

### 5. MikroTik (only if new region or ECMP)
```
# If kz-04 should be in ECMP pool for vpn-route-kz:
/ip route add dst-address=0.0.0.0/0 gateway=172.20.70.24 routing-table=vpn-route-kz distance=1
# That's it. Overlay already reachable via existing master tunnel.
```

## Comparison: Current vs New

| Aspect | Current (hacks) | mesh-router |
|--------|----------------|-------------|
| Docker networks per relay | 3+ (default + per-destination) | 2 (default + mesh) |
| wg-easy WG_POST_UP | 6+ ip rule/route commands | 1 command: route overlay via mesh-router |
| wg-easy network interfaces | 3+ eth + wg0 | 2 eth + wg0 |
| Add endpoint | New network + wg-easy config + PostUp | docker-compose add + up. Router auto-discovers |
| Failover | None | Healthcheck + auto-remove/re-add routes |
| ECMP | Impossible | Labels: same region, same weight |
| Debugging | ip rule/route across 3 containers | GET /routes on mesh-router |
| Return path | Manual PostUp in each wg-client | mesh-router handles |
| Config format | env vars with || true | Docker labels (declarative) |

## Implementation Plan

1. **mesh-router Go service** — Docker API watcher, route manager, healthcheck
2. **Docker image** — Alpine + mesh-router binary + iproute2 + iptables
3. **CI/CD** — GitHub Actions → GHCR
4. **Updated docker-compose templates** — master node compose with mesh-router
5. **Migration** — deploy on RU-01 first, test, then RU-02
6. **MikroTik update** — reduce containers from 5 to 2, update routing tables

## Load Balancing

### Two-Level ECMP with Sticky Sessions

```
Level 1: MikroTik ECMP across masters (transport)
  172.20.70.0/24 via ru-01 distance=1
  172.20.70.0/24 via ru-02 distance=1
  172.20.70.0/24 via ru-03 distance=1  ← add relay = add route
  Hash: src-ip + dst-ip + src-port + dst-port → same flow, same relay

Level 2: mesh-router ECMP across endpoint nodes (destination)
  172.20.70.20 nexthop via .80.21 weight 1
               nexthop via .80.22 weight 1
               nexthop via .80.23 weight 1
  Hash: Linux kernel multipath → same flow, same endpoint
```

Both levels use hash-based distribution = sticky sessions built-in.

### Per-Node IP vs Balancer IP

Two types of overlay addresses:

```
.X0 = region BALANCER — traffic distributed across all nodes in region
.X1-.X9 = individual NODE — traffic pinned to specific node
```

**Balancer IP example:**
```
172.20.70.20 = "any KZ node"
  mesh-router builds ECMP:
    nexthop via 172.20.80.21 (kz-01) weight 1
    nexthop via 172.20.80.22 (kz-02) weight 1
    nexthop via 172.20.80.23 (kz-03) weight 1

  Healthcheck removes dead nodes from pool automatically.
```

**Per-node IP example:**
```
172.20.70.23 = "exactly kz-03"
  mesh-router builds direct route:
    172.20.70.23/32 via 172.20.80.23
```

**MikroTik usage:**
```
# YouTube → any KZ (balanced across kz-01, kz-02, kz-03)
0.0.0.0/0 gateway=172.20.70.20 table=vpn-route-kz distance=1

# Real Debrid → pinned to kz-03
0.0.0.0/0 gateway=172.20.70.23 table=vpn-route-kz-03 distance=1

# AI services → any KZ (balanced)
0.0.0.0/0 gateway=172.20.70.20 table=vpn-route-ai distance=1

# TikTok → any US (balanced, only us-01 for now)
0.0.0.0/0 gateway=172.20.70.40 table=vpn-route-us distance=1
```

### Balancer IP — mesh-router label

```yaml
wg-client-kz-01:
  labels:
    - mesh.enable=true
    - mesh.overlay-ip=172.20.70.21       # per-node IP
    - mesh.balancer-ip=172.20.70.20      # participate in KZ balancer
    - mesh.region=kz
    - mesh.node=kz-01
    - mesh.weight=100                     # ECMP weight (default 100)
```

mesh-router logic:
```go
// For each peer with mesh.balancer-ip:
//   Collect all peers sharing the same balancer-ip
//   Build multipath route with weights
//
// ip route replace 172.20.70.20/32 \
//   nexthop via 172.20.80.21 weight 100 \
//   nexthop via 172.20.80.22 weight 100 \
//   nexthop via 172.20.80.23 weight 100
//
// On peer health change: rebuild multipath route excluding dead peers
```

### Updated Overlay Address Space

```
172.20.70.0/24 — mesh overlay

  .1    = MikroTik gateway

  RU transit:
  .10   = RU balancer (all masters)    ← future use
  .11   = ru-01 (141.98.191.38)
  .12   = ru-02 (147.45.185.141)

  KZ endpoints:
  .20   = KZ balancer (ECMP: .21 + .22 + .23)
  .21   = kz-01 (176.12.75.213)
  .22   = kz-02 (38.180.37.82)
  .23   = kz-03 (176.100.42.175)

  PL endpoints:
  .30   = PL balancer (ECMP: .31)
  .31   = pl-01 (37.252.11.125)

  US endpoints:
  .40   = US balancer (ECMP: .41)
  .41   = us-01 (103.113.70.106)

  .50-.99 = reserved for future regions
```

### Adding a New RU Transport Node

```
1. Deploy AWG node on ru-03 (generic template)
2. On ru-03: docker-compose with mesh-router + wg-client-* for ALL endpoints
   → Each wg-client has labels → mesh-router auto-builds routes
3. On each endpoint: create AWG peer for ru-03
4. On MikroTik:
   /interface/wireguard/peers add ... (AWG to ru-03)
   /ip route add dst-address=172.20.70.0/24 gateway=<ru-03-tunnel> distance=1
5. ECMP kicks in automatically. Traffic distributed across 3 relays.
```

### Bandwidth Utilization

```
3 masters × ~200 Mbps each = ~600 Mbps aggregate
ECMP hash distributes per-flow (not per-packet → no reordering)
Each flow sticks to: one relay × one endpoint
```

## Updated Labels API

| Label | Required | Type | Default | Example | Purpose |
|-------|----------|------|---------|---------|---------|
| `mesh.enable` | yes | bool | — | `true` | Register in mesh |
| `mesh.overlay-ip` | yes | IP | — | `172.20.70.21` | Per-node overlay IP |
| `mesh.balancer-ip` | no | IP | — | `172.20.70.20` | Region balancer (ECMP pool) |
| `mesh.region` | no | string | — | `kz` | Region grouping |
| `mesh.node` | no | string | — | `kz-01` | Node identifier |
| `mesh.weight` | no | int | `100` | `50` | ECMP weight in balancer |
| `mesh.backup` | no | bool | `false` | `true` | Higher metric (failover only) |
| `mesh.healthcheck` | no | string | `ping` | `ping\|tcp\|none` | Health check type |
| `mesh.healthcheck-target` | no | IP | overlay-ip | `172.20.70.21` | What to check |
| `mesh.nat` | no | bool | `false` | `true` | SNAT on mesh-router |

## AWG Parameter Rotation (anti-DPI)

### Existing Work: amneziawg-scripts (D:\Dev\amneziawg-scripts)

Python CLI (`awg_gen.py`) already implements:
- **Capture**: real TLS/QUIC packets from popular sites → data/*.bin
- **Generate**: AWG obfuscation params from captures (9 protocol families, 3 presets)
- **Push**: deploy to wg-easy via API (interface + userconfig)
- **Client create**: per-client unique Jc/Jmin/Jmax/I1-I5 (different fingerprint per peer)
- **UAPI rotation verified**: amneziawg-go supports runtime SET for ALL params — zero downtime

### Integration with mesh-router

mesh-router (or a companion component) should:

1. **Generate** — AWG keys + obfuscation params (Jc, Jmin, Jmax, S1-S4, H1-H4, I1-I5)
   - Reuse awg_gen.py logic or port to Go
   - Use captured packet data for realistic protocol mimicry

2. **Rotate** — periodic schedule (configurable) + on-demand trigger
   - Schedule: e.g., every 24h, randomized within window
   - Trigger: manual API call or health event
   - Coordinate both sides of tunnel simultaneously

3. **Sync** — coordinated between relay wg-client and endpoint wg-easy
   - Critical: both sides must apply new params atomically
   - Tier 2 rotation problem: S/H change while client offline = can't reconnect
   - Solution research exists: multi-config fallback, HTTPS out-of-band sync

4. **Apply** — zero downtime via UAPI (verified: amneziawg-go supports it)
   - No container restart needed
   - Jipok/wgctrl-go library for Go UAPI access

### Key Finding: UAPI Dynamic Rotation

From research (amneziawg-scripts/.agent/data/uapi-dynamic-rotation.md):
- amneziawg-go supports runtime SET for ALL params via UAPI
- Zero downtime rotation is possible without tunnel reconnection
- Jipok/wgctrl-go provides Go bindings for this

### Parameter Split

| Parameter | Scope | Rotation risk |
|-----------|-------|--------------|
| S1-S4 | Server (shared) | HIGH — all clients must update simultaneously |
| H1-H4 | Server (shared) | HIGH — same as S |
| Jc, Jmin, Jmax | Per-client | LOW — only one tunnel endpoint pair |
| I1-I5 | Per-client | LOW — only one tunnel endpoint pair |

Per-client params (J, I) can rotate independently per tunnel.
Server params (S, H) require coordinated rotation across all clients.

## Topology Sync & Source of Truth

### Problem

Multiple masters must have identical wg-client configurations.
Adding endpoint = manual change on every relay. No sync mechanism.

### Solution: Topology File as Source of Truth

```yaml
# mesh-topology.yml (in git, one file for entire mesh)
overlay_space: 172.20.70.0/24
mesh_network: 172.20.80.0/24

relays:
  - name: ru-01
    host: 141.98.191.38
    overlay-ip: 172.20.70.11
  - name: ru-02
    host: 147.45.185.141
    overlay-ip: 172.20.70.12

endpoints:
  - name: kz-01
    host: 176.12.75.213
    port: 853
    overlay-ip: 172.20.70.21
    balancer-ip: 172.20.70.20
    region: kz
  - name: kz-02
    host: 38.180.37.82
    port: 853
    overlay-ip: 172.20.70.22
    balancer-ip: 172.20.70.20
    region: kz
  - name: kz-03
    host: 176.100.42.175
    port: 853
    overlay-ip: 172.20.70.23
    balancer-ip: 172.20.70.20
    region: kz
  - name: pl-01
    host: 37.252.11.125
    port: 853
    overlay-ip: 172.20.70.31
    balancer-ip: 172.20.70.30
    region: pl
  - name: us-01
    host: 103.113.70.106
    port: 853
    overlay-ip: 172.20.70.41
    balancer-ip: 172.20.70.40
    region: us
```

### Sync mechanism

mesh-router on each relay:
1. Reads topology (from file, git pull, or HTTP endpoint)
2. Compares with running containers (via Docker API)
3. Creates missing wg-client containers (with correct labels)
4. Removes orphaned containers
5. Generates AWG configs for new tunnels (key exchange with endpoint)
6. Polls for topology changes (configurable interval or webhook)

### Key exchange workflow (adding endpoint kz-04)

```
1. Add kz-04 to mesh-topology.yml, push to git
2. mesh-router on ru-01 detects change (poll or webhook)
3. mesh-router calls kz-04 wg-easy API:
   - POST /api/client → creates peer, gets public key + config
   - Stores config in /srv/wg-client-kz-04/wg0.conf
4. mesh-router creates wg-client-kz-04 container (with labels)
5. Route auto-added from labels
6. mesh-router on ru-02 does the same independently
7. No manual steps on any relay.
```

### Requirements
- Maximum simplicity
- Maximum efficiency
- Maximum independence from external stacks (no Consul, no etcd, no k8s)
- Topology file = plain YAML in git
- Key exchange = wg-easy REST API (already exists and tested)

## Terminology

| Term | Role | Description |
|------|------|-------------|
| **master node** | ingress | Receives traffic from MikroTik, routes into mesh. Runs mesh-router + wg-clients. |
| **endpoint node** | egress | Exits traffic to internet. Runs wg-easy + NAT. |
| **mesh-router** | data plane | Per-master container. Watches labels, builds routes, healthcheck. Standalone — no inter-master communication. |
| **mesh-ctl** | control plane | CLI on admin machine. Manages topology, deploys to masters, rotates keys. Like Terraform for mesh. |
| **overlay IP** | identity | 172.20.70.X — node's routable mesh address |
| **balancer IP** | virtual | 172.20.70.X0 — region ECMP pool |

## Architecture: Control Plane vs Data Plane

### Decision: Independent Masters + External CLI (Terraform-like)

**Rejected:** masters communicate and sync with each other.
- Distributed state = distributed problems (split-brain, conflicts, staleness)
- Unnecessary complexity — MikroTik already handles failover via ECMP
- Inter-master dependency: one misbehaving master affects others

**Approved:** each master is standalone, managed by external CLI.

```
Control plane (admin machine):       Data plane (per master, standalone):
┌──────────────────────┐            ┌────────────────────────┐
│ mesh-ctl CLI         │   SSH/API  │ master-01              │
│ mesh-topology.yml    │──────────→ │   mesh-router          │
│ git repo             │            │   wg-client-kz-01      │
│ AWG key management   │            │   wg-client-kz-02      │
│ rotation scheduler   │            │   wg-client-pl-01      │
└──────────┬───────────┘            │   wg-easy (MikroTik)   │
           │                        │   (no knowledge of     │
           │                        │    other masters)       │
           │           SSH/API      └────────────────────────┘
           │                        ┌────────────────────────┐
           └──────────────────────→ │ master-02              │
                                    │   mesh-router          │
                                    │   wg-client-kz-01      │
                                    │   wg-client-kz-02      │
                                    │   wg-client-pl-01      │
                                    │   wg-easy (MikroTik)   │
                                    │   (standalone)         │
                                    └────────────────────────┘
```

### Why this works

- **Master dies** → MikroTik removes ECMP route (check-gateway=ping). Other masters unaffected.
- **Add endpoint** → `mesh-ctl add-endpoint kz-04` pushes to ALL masters in sequence.
- **Rotate params** → `mesh-ctl rotate` calls each master independently.
- **No distributed state** → topology.yml in git is the ONLY source of truth.
- **No inter-master networking** → masters don't need to see each other at all.

### mesh-router responsibilities (data plane ONLY)

1. Watch Docker labels → build Linux routing table
2. Healthcheck overlay IPs → remove/re-add routes
3. NAT (if needed)
4. HTTP API for status/debugging

Does NOT: pull topology, sync with other masters, manage containers, generate keys.

### mesh-ctl responsibilities (control plane)

```bash
mesh-ctl status                         # show mesh health from all masters
mesh-ctl apply                          # push topology to all masters
mesh-ctl add-endpoint kz-04             # add node: keygen + deploy to all masters
mesh-ctl remove-endpoint kz-04          # remove from all masters
mesh-ctl rotate                         # rotate AWG params on all tunnels
mesh-ctl rotate --node kz-01            # rotate specific tunnel
mesh-ctl rotate --schedule 24h          # schedule periodic rotation
mesh-ctl capture -f domains.txt         # capture TLS/QUIC packets (from awg_gen)
mesh-ctl keygen                         # generate new keypair
mesh-ctl ssh master-01 -- ip route      # run command on specific master
```

### mesh-ctl internals

- Reads `mesh-topology.yml` (local file, in git)
- For each master in topology: SSH + Docker API (or HTTP API on mesh-router)
- Key generation: reuses awg_gen.py logic (Go port or subprocess)
- AWG param generation: reuses awg_gen.py capture data + protocol families
- State: topology.yml + generated keys (encrypted in git or local vault)
- No daemon — runs on demand like terraform/ansible

### Per-master key isolation

Each master node has its OWN set of AWG keys and obfuscation params for each endpoint:
```
master-01 ↔ kz-01: keypair A, params A
master-01 ↔ kz-02: keypair B, params B
master-02 ↔ kz-01: keypair C, params C  ← DIFFERENT from master-01
master-02 ↔ kz-02: keypair D, params D  ← DIFFERENT from master-01
```

This means:
- Compromising one master doesn't expose other masters' tunnels
- DPI can't correlate traffic across masters (different fingerprints)
- Each tunnel can rotate independently

## Open Design Questions

1. **NAT location:** endpoint node SNAT preferred (each node NATs its own exit traffic). mesh-router NAT only for special cases.
2. **Return path symmetry:** guaranteed by AWG — stateful tunnel, response returns via same path.
3. **mesh-router HA:** `restart: always` + Docker healthcheck. If mesh-router restarts, routes rebuild in seconds from Docker labels. No VRRP needed for single-host.
4. **Metrics/observability:** Prometheus endpoint preferred. Expose: peer count, peer health, route count, packets forwarded per peer.
5. **Rotation coordination:** Tier 2 (S/H params) requires all clients on that endpoint to update — mesh-ctl handles sequencing. Tier 1 (J/I params) is per-tunnel, simpler.
6. **mesh-ctl transport:** SSH + docker compose? Or HTTP API on mesh-router? SSH is simpler, API is cleaner. Could support both.
