# Implementation Plan: AWG Mesh

**Spec:** .agent/specs/awg-mesh/spec.md
**Created:** 2026-03-26
**Status:** Draft

## Tech Stack

| Component | Choice | Rationale |
|-----------|--------|-----------|
| Language | Go 1.24+ | amneziawg-go ecosystem, single static binary, no runtime deps |
| AWG UAPI | Jipok/wgctrl-go | Go bindings for WireGuard UAPI, supports all AWG params. Verified in amneziawg-scripts research. |
| AWG userspace | amnezia-vpn/amneziawg-go | WireGuard-Go fork with DPI obfuscation. Runs in container. |
| gRPC | google.golang.org/grpc v1.73+ | High reputation, mTLS native, streaming support |
| Protobuf | google.golang.org/protobuf | Standard Go protobuf runtime |
| CLI | spf13/cobra | De facto Go CLI standard, subcommand support |
| Config | gopkg.in/yaml.v3 | YAML topology parsing |
| Packet capture | gopacket | Go port of libpcap, TLS/QUIC capture |
| Prometheus | prometheus/client_golang | Built-in metrics endpoint |
| Logging | rs/zerolog | Structured JSON logging, zero-alloc |
| Docker | docker/docker client SDK | Container management for MikroTik command generation |
| TLS/Crypto | crypto/tls, crypto/x509 (stdlib) | mTLS CA, cert generation, no external deps |
| IP/Net | net, netip (stdlib) | CIDR parsing, overlay address management |

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                     CONTROL PLANE                           │
│                                                             │
│  mesh-ctl (Go CLI on admin PC)                              │
│  ├── topology.yml (source of truth)                         │
│  ├── ~/.mesh-ctl/ca.key (mesh CA)                           │
│  ├── ~/.mesh-ctl/nodes/<name>/token                         │
│  └── gRPC client (mTLS + token fallback)                    │
│                          │                                  │
│           ┌──────────────┼──────────────┐                   │
│           ▼              ▼              ▼                    │
│     ┌──────────┐  ┌──────────┐  ┌──────────┐               │
│     │ master-01│  │ master-02│  │  kz-01   │               │
│     │ (gRPC)   │  │ (gRPC)   │  │  (gRPC)  │               │
│     └──────────┘  └──────────┘  └──────────┘               │
└─────────────────────────────────────────────────────────────┘

┌─────────────────────────────────────────────────────────────┐
│                      DATA PLANE                             │
│                                                             │
│  awg-mesh-node --mode master (per master host)              │
│  ├── gRPC server (:9090, mTLS + token)                      │
│  ├── AWG interfaces (amneziawg-go UAPI)                     │
│  │   ├── wg-ingress (server for MikroTik/clients)           │
│  │   ├── wg-kz-01 .. wg-kz-03 (tunnels to endpoints)       │
│  │   └── wg-pl-01, wg-us-01                                 │
│  ├── IP router (overlay routes + ECMP balancers)            │
│  ├── Healthcheck (ping overlay IPs)                         │
│  ├── Capture engine (gopacket TLS/QUIC)                     │
│  ├── AWG param generator (pkg/awggen)                       │
│  └── Prometheus (:9091/metrics)                             │
│                                                             │
│  awg-mesh-node --mode endpoint (per endpoint host)          │
│  ├── gRPC server (:9090)                                    │
│  ├── AWG interface wg0 (server, accepts master peers)       │
│  ├── NAT (iptables MASQUERADE)                              │
│  ├── Overlay IP (loopback)                                  │
│  └── Prometheus (:9091/metrics)                             │
│                                                             │
│  awg-mesh-node --mode client (MikroTik / Linux)             │
│  ├── gRPC server (:9090)                                    │
│  ├── AWG interfaces (tunnels to masters)                    │
│  └── Overlay routing                                        │
└─────────────────────────────────────────────────────────────┘
```

## Data Model

### Topology (mesh-topology.yml)

```yaml
overlay:
  space: "172.20.70.0/24"          # CIDR
  physical_mtu: 1500
  awg_overhead: 60
  ranges: []                        # NamedRange[]

masters: []                         # MasterNode[]
endpoints: []                       # EndpointNode[]
clients: []                         # ClientNode[]

capture:
  domains_file: "domains.txt"
  schedule: "0 3 * * *"
  retention_days: 7

rotation:
  defaults:
    tier1_interval: "24h"
    tier2_interval: "7d"
    tier3_interval: "30d"
    preset: "aggressive"
```

### Node State (on-disk /config/)

```
/config/
  node.yml              # role, overlay-ip, name
  tls/
    ca.crt              # mesh CA certificate
    node.crt            # node mTLS certificate
    node.key            # node mTLS private key
  wg/
    wg-ingress.conf     # AWG interface configs
    wg-kz-01.conf
    wg-kz-02.conf
    ...
  token                 # MESH_TOKEN (hashed)
  capture-data/         # .bin files (master only)
    tls_yandex_ru_001.bin
    quic_vk_com_001.bin
```

### mesh-ctl Local State

```
~/.mesh-ctl/
  ca.key                # CA private key (0600)
  ca.crt                # CA certificate
  nodes/
    master-01/
      token             # MESH_TOKEN for this node
      host              # IP:port
    kz-01/
      token
      host
```

## API Contracts (gRPC)

### proto/agent.proto

```protobuf
syntax = "proto3";
package awgmesh;

service AwgAgent {
  // Onboarding
  rpc Init(InitRequest) returns (InitResponse);
  rpc RotateToken(RotateTokenRequest) returns (RotateTokenResponse);

  // Tunnel management (master mode)
  rpc AddTunnel(AddTunnelRequest) returns (AddTunnelResponse);
  rpc RemoveTunnel(RemoveTunnelRequest) returns (RemoveTunnelResponse);
  rpc ListTunnels(Empty) returns (TunnelList);

  // Peer management (endpoint mode)
  rpc AddPeer(AddPeerRequest) returns (AddPeerResponse);
  rpc RemovePeer(RemovePeerRequest) returns (RemovePeerResponse);
  rpc ListPeers(Empty) returns (PeerList);

  // Rotation
  rpc RotateParams(RotateParamsRequest) returns (RotateParamsResponse);
  rpc GetParams(GetParamsRequest) returns (AwgParams);

  // Capture (master only)
  rpc CaptureRefresh(CaptureRequest) returns (CaptureResponse);

  // Status
  rpc GetStatus(Empty) returns (NodeStatus);
  rpc GetRoutes(Empty) returns (RouteTable);         // master only
  rpc GetHealth(Empty) returns (HealthResponse);
}

message InitRequest {
  bytes ca_cert = 1;
  bytes node_cert = 2;
  bytes node_key = 3;
  NodeConfig config = 4;            // role, overlay-ip, peers
}

message AddTunnelRequest {
  string name = 1;                  // "kz-04"
  string endpoint_host = 2;         // "1.2.3.4:853"
  string overlay_ip = 3;            // "172.20.70.24"
  string balancer_ip = 4;           // "172.20.70.16"
  bytes peer_public_key = 5;
  bytes preshared_key = 6;
  AwgParams params = 7;
  int32 weight = 8;                 // ECMP weight
  bool backup = 9;
}

message RotateParamsRequest {
  string tunnel_name = 1;           // which tunnel to rotate
  int32 tier = 2;                   // 1, 2, or 3
  AwgParams new_params = 3;         // pre-generated by mesh-ctl
  bytes new_public_key = 4;         // tier 3 only
}

message AwgParams {
  int32 jc = 1;
  int32 jmin = 2;
  int32 jmax = 3;
  int32 s1 = 4;
  int32 s2 = 5;
  int32 s3 = 6;
  int32 s4 = 7;
  int32 h1 = 8;
  int32 h2 = 9;
  int32 h3 = 10;
  int32 h4 = 11;
  string i1 = 12;                   // I-spec encoded string
  string i2 = 13;
  string i3 = 14;
  string i4 = 15;
  string i5 = 16;
}

message NodeStatus {
  string name = 1;
  string mode = 2;                  // master/endpoint/client
  string overlay_ip = 3;
  repeated TunnelStatus tunnels = 4;
  string uptime = 5;
}

message TunnelStatus {
  string name = 1;
  string overlay_ip = 2;
  bool healthy = 3;
  int64 last_handshake = 4;
  int64 tx_bytes = 5;
  int64 rx_bytes = 6;
  string last_rotation = 7;
}
```

## File Structure

```
D:\Dev\awg-mesh/
├── cmd/
│   ├── awg-mesh-node/            # unified node binary
│   │   └── main.go
│   └── mesh-ctl/                 # CLI control plane
│       ├── main.go
│       └── cmd/                  # cobra commands
│           ├── root.go
│           ├── endpoint.go       # prepare/init/remove
│           ├── master.go
│           ├── client.go
│           ├── rotate.go
│           ├── capture.go
│           ├── token.go
│           ├── ip.go             # range management
│           ├── status.go
│           └── bootstrap.go
├── pkg/
│   ├── awggen/                   # AWG param generation (port of awg_gen.py)
│   │   ├── capture.go            # TLS/QUIC packet capture
│   │   ├── families.go           # 9 protocol families
│   │   ├── generator.go          # param generation with presets
│   │   ├── ispec.go              # I-spec tag encoding
│   │   ├── presets.go            # aggressive/balanced/minimal
│   │   └── mtu.go                # MTU validation, S3/S4 constraints
│   ├── node/                     # awg-mesh-node core logic
│   │   ├── node.go               # main node struct, mode dispatch
│   │   ├── master.go             # master-specific: routing, ECMP, tunnels
│   │   ├── endpoint.go           # endpoint-specific: NAT, peer accept
│   │   ├── client.go             # client-specific: tunnel to masters
│   │   ├── health.go             # healthcheck loop
│   │   └── config.go             # node config load/save
│   ├── wg/                       # WireGuard/AWG interface management
│   │   ├── interface.go          # create/destroy AWG interfaces
│   │   ├── uapi.go               # UAPI client (via Jipok/wgctrl-go)
│   │   ├── config.go             # conf file generation
│   │   └── keygen.go             # key generation
│   ├── grpc/                     # gRPC server + client
│   │   ├── server.go             # dual auth: mTLS + token
│   │   ├── client.go             # mesh-ctl client
│   │   └── auth.go               # interceptors: mTLS, token validation
│   ├── tls/                      # mTLS certificate management
│   │   ├── ca.go                 # CA generation, cert issuance
│   │   ├── cert.go               # cert load/save/validate
│   │   └── token.go              # MESH_TOKEN generation + rotation
│   ├── topology/                 # topology.yml parser + validator
│   │   ├── topology.go           # types + parsing
│   │   ├── ranges.go             # CIDR range management
│   │   ├── validate.go           # overlap check, orphan check
│   │   └── allocator.go          # IP auto-allocation
│   ├── routing/                  # Linux routing management
│   │   ├── route.go              # ip route add/del/replace
│   │   ├── ecmp.go               # multipath ECMP routes
│   │   ├── nat.go                # iptables NAT rules
│   │   └── mss.go                # TCP MSS clamping
│   └── mikrotik/                 # MikroTik command generation
│       ├── commands.go           # /container, /interface/veth, /ip/route
│       └── templates.go          # .rsc script templates
├── proto/
│   ├── agent.proto               # AwgAgent service
│   └── types.proto               # shared message types
├── deploy/
│   ├── Dockerfile                # multi-stage: build + alpine runtime
│   └── templates/
│       ├── docker-compose.master.yml.tmpl
│       ├── docker-compose.endpoint.yml.tmpl
│       └── docker-compose.client.yml.tmpl
├── .github/
│   └── workflows/
│       └── build.yml             # build + push to ghcr.io
├── .agent/
├── CLAUDE.md
├── AGENTS.md
├── go.mod
├── go.sum
├── Makefile                      # build, test, proto-gen, docker
└── mesh-topology.example.yml
```

## Phases

### Phase 1: Foundation (FR-1 partial, FR-2)

**Goal:** Go module, core types, AWG interface management, single tunnel proof-of-concept.

**Deliverables:**
- Go module initialized, CI green
- `pkg/wg/` — create AWG interface, set params via UAPI, establish tunnel
- `pkg/topology/` — parse topology.yml, CIDR ranges, IP allocation
- `cmd/awg-mesh-node/` — skeleton, starts single AWG tunnel from config file
- One tunnel working: awg-mesh-node (server) ↔ awg-mesh-node (client)
- Dockerfile, build + push to GHCR

**Validates:** amneziawg-go + Jipok/wgctrl-go integration, UAPI param set, tunnel establishment

### Phase 2: gRPC + Auth (FR-10, FR-9 partial)

**Goal:** gRPC server with dual auth (mTLS + token), mesh-ctl init workflow.

**Deliverables:**
- `proto/agent.proto` — full service definition, protoc generation
- `pkg/grpc/` — server with dual auth interceptor
- `pkg/tls/` — CA generation, cert issuance, token management
- `cmd/mesh-ctl/` — cobra skeleton, `init` subcommand
- `mesh-ctl <role> prepare` generates docker-compose + token
- `mesh-ctl <role> init` connects via token, exchanges certs, activates mTLS
- Token rotation: `mesh-ctl token rotate`

**Validates:** full onboarding flow (prepare → deploy → init)

### Phase 3: Master Mode (FR-1, FR-4, FR-5, FR-13)

**Goal:** Master node manages multiple tunnels, routes, ECMP, healthcheck.

**Deliverables:**
- `pkg/node/master.go` — multi-tunnel management via gRPC AddTunnel/RemoveTunnel
- `pkg/routing/` — overlay routes, ECMP balancer IPs, TCP MSS clamping
- `pkg/node/health.go` — ping-based healthcheck, auto-remove/re-add
- Auto MTU calculation based on hop count
- `mesh-ctl endpoint prepare/init/remove` — full lifecycle
- `mesh-ctl master prepare/init/remove` — full lifecycle
- `mesh-ctl status` — mesh overview

**Validates:** full master↔endpoint connectivity, failover, ECMP balancing

### Phase 4: AWG Param Generation + Rotation (FR-6, FR-7, FR-8, FR-14)

**Goal:** Port awg_gen.py to Go, implement rotation protocol.

**Deliverables:**
- `pkg/awggen/` — full Go port: 9 protocol families, 3 presets, I-spec tags, MTU validation
- `pkg/awggen/capture.go` — TLS/QUIC capture via gopacket
- Tier 1 rotation: `mesh-ctl rotate --tier 1` (zero downtime via UAPI)
- Tier 2 rotation: coordinated across masters
- Tier 3 rotation: keypair change with brief reconnect
- Configurable schedule in topology.yml
- `mesh-ctl capture refresh` — capture from domain list
- Per-master independent capture data
- Per-master unique AWG params per tunnel (FR-14)

**Validates:** rotation without packet loss, capture data diversity

### Phase 5: Client + MikroTik (FR-12, FR-3, FR-11)

**Goal:** Client onboarding, MikroTik support, address space management.

**Deliverables:**
- `cmd/mesh-ctl/cmd/client.go` — prepare/init/remove for clients
- `pkg/mikrotik/` — RouterOS command generation, .rsc scripts
- MikroTik container deploy instructions + sequential rotation
- `mesh-ctl ip list/range add/resize/move` — address space management
- `mesh-ctl rotate --client` — MikroTik rotation with ECMP coverage
- Full topology.yml example with all features

**Validates:** MikroTik integration end-to-end, address space operations

### Phase 6: Observability + Polish (NFR-6, NFR-4)

**Goal:** Production readiness.

**Deliverables:**
- Prometheus metrics endpoint (:9091/metrics)
- Structured JSON logging (zerolog) on all components
- `mesh-ctl capture schedule` — cron setup
- `mesh-ctl rotate --scheduled` — scheduled rotation runner
- Comprehensive error handling + graceful shutdown
- README.md, AGENTS.md, topology reference
- Migration guide from current MikroTik setup

**Validates:** observability, operability, documentation completeness

## Library Decisions

| Component | Library | Version | Rationale |
|-----------|---------|---------|-----------|
| AWG UAPI | Jipok/wgctrl-go | latest | Config struct has ALL AWG fields: Jc/Jmin/Jmax *int, S1-S4, H1-H4 *int, I1-I5 *string. Has GenerateAmneziaParams() as fallback. Library mode: direct Device.IpcSet(). Verified. |
| AWG userspace | amnezia-vpn/amneziawg-go | latest | Used as Go LIBRARY (import device/tun/conn), NOT as separate binary. One process manages N AWG interfaces via N Device instances. No patches needed. |
| gRPC | google.golang.org/grpc | v1.73+ | Standard, high perf, native mTLS, streaming. |
| CLI | spf13/cobra | v1.8+ | De facto Go CLI. Subcommands, help gen, completions. |
| Config | gopkg.in/yaml.v3 | v3 | Standard YAML parser. No external YAML alternatives needed. |
| Packet capture | gopacket | latest | Go libpcap. For TLS ClientHello + QUIC Initial capture. |
| Logging | rs/zerolog | latest | Zero-alloc structured JSON. Lighter than zap. |
| Metrics | prometheus/client_golang | latest | Standard Prometheus. |
| IP math | net/netip (stdlib) | — | CIDR parsing, address allocation. No external dep. |
| Crypto | crypto/* (stdlib) | — | CA, certs, keys. No external dep. |
| Docker/MikroTik | Custom | — | RouterOS command generation is template-based, no SDK needed. |

## Unknowns and Risks

| Unknown | Impact | Resolution Strategy |
|---------|--------|-------------------|
| gopacket on Alpine Docker | MEDIUM | Test in Phase 4. Fallback: capture on host, mount to container. |
| amneziawg-go multi-interface per process | ~~HIGH~~ **RESOLVED** | Verified: `device.NewDevice()` is stateless — no global state, independent goroutines/queues/peers per Device. N devices in one process confirmed. Import as Go library, not exec as separate binary. No patches needed. |
| UAPI param rotation atomicity | MEDIUM | Verify in Phase 1. If not atomic: accept brief mismatch window (AWG handles gracefully). |
| MikroTik container gRPC connectivity | MEDIUM | Verify in Phase 5. Port mapping on RouterOS container. Fallback: REST API via Traefik. |
| ~~Jipok/wgctrl-go AWG param completeness~~ | ~~HIGH~~ **RESOLVED** | All AWG params (Jc/Jmin/Jmax, S1-S4, H1-H4, I1-I5) confirmed in wgtypes.Config. Blueprint verified. |
| Tier 2 rotation: offline client deadlock | HIGH | Server changes S/H → offline client locked out. Mitigation: mesh-ctl checks ALL masters healthy before Tier 2. Future: multi-config fallback (H1-H4 plaintext → server identifies client's param set without decryption). |
| gopacket in Docker Alpine | MEDIUM | Needs libpcap. Fallback: capture on host, mount data to container. Test in Phase 4. |

## FR → Phase Mapping

| FR | Phase | Notes |
|----|-------|-------|
| FR-1 (Unified binary) | 1,3 | Skeleton in P1, modes in P3 |
| FR-2 (Topology as code) | 1 | Types + parsing |
| FR-3 (Address space) | 5 | Range management CLI |
| FR-4 (ECMP balancing) | 3 | Routing + balancer IPs |
| FR-5 (Healthcheck failover) | 3 | Ping-based health loop |
| FR-6 (Rotation) | 4 | All 3 tiers |
| FR-7 (Param generation) | 4 | Go port of awg_gen.py |
| FR-8 (Daily capture) | 4 | gopacket capture |
| FR-9 (Onboarding) | 2 | prepare → deploy → init |
| FR-10 (gRPC API) | 2 | Full service + dual auth |
| FR-11 (mesh-ctl CLI) | 2-5 | Incremental across phases |
| FR-12 (MikroTik) | 5 | RouterOS commands + rotation |
| FR-13 (MTU) | 3 | Auto-calc + clamp |
| FR-14 (Key isolation) | 4 | Per-master unique params |

## NFR Coverage

| NFR | Approach |
|-----|----------|
| NFR-1 (Performance) | UAPI rotation = sub-second. Health interval configurable. |
| NFR-2 (Security) | mTLS + token dual auth. WG crypto. CA-based trust. |
| NFR-3 (Reliability) | Persistent /config volume. Auto-reconnect. Independent masters. |
| NFR-4 (Simplicity) | 1 binary, 1 YAML, 3-step onboarding, no distributed state. |
| NFR-5 (Portability) | Static Go binary, Alpine Docker, MikroTik container. |
| NFR-6 (Observability) | Prometheus + zerolog. Phase 6. |
