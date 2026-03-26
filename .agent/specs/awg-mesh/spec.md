# Feature: AWG Mesh — Docker-native Encrypted Overlay Network

**Slug:** awg-mesh
**Created:** 2026-03-26
**Status:** Clarified
**Author:** AI Agent (reviewed by user)
**Design docs:** `.agent/data/mesh-router-design.md`, `mesh-processes.md`, `mesh-onboarding.md`, `mesh-unified-node.md`, `mesh-address-space.md`

## Overview

A self-contained encrypted mesh network built on AmneziaWG (WireGuard fork with DPI obfuscation). Replaces the current ad-hoc MikroTik container routing (5 containers, manual config, no failover, relay hacks) with a clean architecture: unified node binary, topology-as-code, automatic key rotation, and region-based load balancing with failover.

## Context

### Current State

MikroTik routes traffic through 5 AmneziaWG containers to 5 endpoint nodes. Each container is a separate veth interface on BridgeDockers. Routing is 1:1 — one address list → one container → one node. Relay through RU nodes uses source-based `ip rule` hacks inside wg-easy, with 6+ `WG_POST_UP` commands per relay, separate Docker networks per destination, and no failover. Adding a node requires creating a new MikroTik container, veth, routing table, and mangle rules.

### Problems

1. **Rigid 1:1 mapping** — one node per routing table, no load balancing
2. **Relay hacking** — source-based ip rule in wg-easy, separate Docker networks per destination
3. **No failover** — node down = traffic black-holed
4. **No key rotation** — static AWG obfuscation params enable DPI pattern learning
5. **Manual everything** — adding a node requires changes on MikroTik + relay + endpoint
6. **No single source of truth** — config scattered across MikroTik, relay compose files, endpoint configs
7. **wg-easy overhead** — web UI, SQLite, session management for what should be pure transport

### Target State

Single Go binary (`awg-mesh-node`) running as one Docker container per host. Topology defined in YAML. Management via CLI (`mesh-ctl`). Automatic key rotation, region-based ECMP balancing, health-checked failover, gRPC management API with mTLS.

## Functional Requirements

### FR-1: Unified Node Binary

The system MUST provide a single binary (`awg-mesh-node`) that operates in three modes:
- **master** (ingress): accepts client connections, creates tunnels to endpoints, routes between them, performs health checks, captures TLS/QUIC packets for AWG param generation
- **endpoint** (egress): accepts tunnels from masters, NATs traffic to internet
- **client**: connects to masters, routes overlay traffic (works identically on Linux Docker and MikroTik container runtime)

### FR-2: Topology as Code

The system MUST use a single YAML file (`mesh-topology.yml`) as the source of truth for the entire mesh. The file MUST define: overlay address space, named IP ranges with CIDR notation, master nodes, endpoint nodes, clients, capture configuration, and rotation schedules.

### FR-3: Overlay Address Space

The system MUST manage a configurable overlay address space (default `/24`) with:
- Named IP ranges defined in CIDR or explicit range notation
- Per-range balancer IP (any IP, not restricted to first in range)
- Auto-assignment of overlay IPs from ranges
- CLI commands to add, resize, move, rename, and delete ranges
- Validation: no overlapping ranges, no orphaned nodes on resize

### FR-4: Region-Based Load Balancing

The system MUST support two-level ECMP with sticky sessions:
- **Level 1** (transport): MikroTik ECMP across master nodes
- **Level 2** (destination): master ECMP across endpoint nodes within a region
- Per-node overlay IP for pinned routing (traffic to specific node)
- Per-region balancer IP for distributed routing (traffic to any node in region)
- Automatic balancer pool updates on node add/remove/health change

### FR-5: Health-Checked Failover

The system MUST perform periodic health checks on all tunnels:
- Configurable interval, timeout, and failure threshold
- Failed tunnel removed from routing table and balancer pool
- Recovered tunnel re-added automatically
- Health state transitions logged

### FR-6: AWG Parameter Rotation

The system MUST support automatic rotation of AWG obfuscation parameters:
- **Tier 1** (per-tunnel): Jc, Jmin, Jmax, I1-I5 — zero downtime via UAPI
- **Tier 2** (per-endpoint): S1-S4, H1-H4 — coordinated across all masters, zero downtime via UAPI
- **Tier 3** (per-tunnel): WireGuard keypair — brief reconnect (~2s)
- Configurable rotation schedule per endpoint
- On-demand rotation via CLI
- Rotation uses captured TLS/QUIC packet data for realistic protocol mimicry

### FR-7: AWG Parameter Generation

The system MUST generate AWG obfuscation parameters from captured network packets:
- Capture TLS ClientHello and QUIC Initial packets from configurable domain list
- 9 protocol families (TLS, QUIC, DNS, STUN, DTLS, NTP, HTTP, WebSocket, TURN)
- 3 presets (aggressive, balanced, minimal)
- I-spec tags: `<b>`, `<r>`, `<rc>`, `<rd>`, `<t>` (NO `<c>` — unsupported by amneziawg-go)
- S3/S4 validation against actual MTU at each hop
- Go port of existing Python logic (amneziawg-scripts `awg_gen.py`)

### FR-8: Daily Capture Data Refresh

The system MUST support periodic capture of TLS/QUIC packets from popular websites:
- Configurable domain list per topology
- Configurable schedule (default: daily)
- Per-master independent capture (different capture data = different fingerprints)
- Retention policy for old captures

### FR-9: Node Onboarding (prepare → deploy → init)

The system MUST follow a three-step onboarding protocol:
1. **prepare**: `mesh-ctl` generates config files + MESH_TOKEN
2. **deploy**: user deploys container manually (Docker or MikroTik)
3. **init**: `mesh-ctl` connects via token, exchanges mTLS certs and configs, mTLS becomes primary auth (token remains as permanent fallback)

Config files MUST require a persistent volume (CONFIG_DIR) — nodes MUST survive container restart.

### FR-10: gRPC Management API

The system MUST expose a gRPC API on each node:
- Authenticated with mTLS (after init) or MESH_TOKEN (permanent fallback)
- Services: peer management, parameter rotation, capture refresh, status/health
- No SSH required for management after bootstrap

### FR-11: CLI Control Plane (mesh-ctl)

The system MUST provide a CLI tool (`mesh-ctl`) for all management operations:
- Endpoint/master/client lifecycle: prepare, init, remove
- Rotation: per-tunnel, per-endpoint, per-client, scheduled
- Capture: refresh, schedule, domain management
- Address space: range management, IP listing
- Status: mesh overview, per-node detail
- Topology file as single source of truth

### FR-12: MikroTik Client Support

The system MUST support MikroTik AWG containers as clients:
- Same awg-mesh-node binary with gRPC + UAPI (MikroTik container runtime is full Docker)
- Generate RouterOS-compatible `/container add` commands and .rsc import scripts
- Sequential rotation with ECMP coverage (one master at a time)
- SMB share for initial config delivery; gRPC for runtime management

### FR-13: Automatic MTU Calculation

The system MUST automatically compute MTU for each tunnel interface based on hop count:
- `hop_mtu = physical_mtu - hop_count * awg_overhead`
- TCP MSS clamping (`--clamp-mss-to-pmtu`) on every AWG interface
- S3/S4 AWG parameter validation against computed MTU
- Per-node MTU override capability

### FR-14: Per-Master Key Isolation

Each master node MUST have its own unique set of AWG keys and obfuscation parameters for each endpoint tunnel. Compromising one master MUST NOT expose other masters' tunnel parameters.

## Non-Functional Requirements

### NFR-1: Performance
- Tunnel establishment: < 5 seconds from container start to first handshake
- Rotation (Tier 1): < 2 seconds total, zero packet loss
- Health check: configurable interval ≥ 5 seconds, detection within 3 intervals
- ECMP hash distribution: within 20% of uniform across masters

### NFR-2: Security
- All management: dual auth — mTLS primary, MESH_TOKEN permanent fallback (always active in parallel)
- All transport: WireGuard Curve25519 + ChaCha20-Poly1305 + optional PSK
- MESH_TOKEN: permanent per-node, rotatable via `mesh-ctl token rotate`, stored on node + admin PC
- No unauthenticated endpoints exposed
- WG private keys stored ONLY on nodes (/config/, 0600). mesh-ctl does not retain private keys.
- CA key stored at ~/.mesh-ctl/ca.key (0600) — single root of trust

### NFR-3: Reliability
- Node restart: automatic tunnel re-establishment from persistent config
- Master failure: MikroTik ECMP removes route within health check interval
- Endpoint failure: masters remove from routing table and balancer pool
- mesh-ctl unavailable: data plane continues operating independently

### NFR-4: Simplicity
- Single binary for all node roles
- Single YAML file for entire topology
- Maximum 3 steps for any node lifecycle operation
- No external dependencies beyond Docker and the binary itself
- No distributed state between masters

### NFR-5: Portability
- Docker container: any Linux host with Docker
- MikroTik: RouterOS container runtime (config-only mode)
- Binary: static Go build, no CGO dependencies

### NFR-6: Observability
- Structured logging (JSON) on all components
- Per-tunnel health status queryable via gRPC
- Rotation event log with timestamps and param hashes
- Optional Prometheus metrics endpoint

## User Stories

### US1: Add Endpoint Node (P1)
**As an** operator, **I want** to add a new endpoint node to the mesh, **so that** I can expand egress capacity in a region.

**Acceptance Criteria:**
- [ ] `mesh-ctl endpoint prepare` generates valid docker-compose + MESH_TOKEN
- [ ] After deploy + `mesh-ctl endpoint init`, node is reachable via overlay IP from all masters
- [ ] Balancer pool for the region includes the new node
- [ ] topology.yml is updated and committed
- [ ] No manual config on any existing node required

### US2: Add Master Node (P1)
**As an** operator, **I want** to add a new master node, **so that** I can increase transport bandwidth and redundancy.

**Acceptance Criteria:**
- [ ] `mesh-ctl master prepare` generates valid docker-compose + MESH_TOKEN
- [ ] After deploy + init, master has tunnels to ALL endpoints
- [ ] MikroTik setup instructions generated (RouterOS commands)
- [ ] MikroTik ECMP route distributes traffic across all masters

### US3: Rotate AWG Parameters (P1)
**As an** operator, **I want** AWG parameters to rotate automatically, **so that** DPI systems cannot learn tunnel fingerprints.

**Acceptance Criteria:**
- [ ] Tier 1 rotation completes with zero packet loss
- [ ] Tier 2 rotation coordinates across all masters before applying
- [ ] Rotation uses fresh capture data (not stale params)
- [ ] Failed rotation rolls back to previous params
- [ ] Rotation event logged with old/new param hashes

### US4: Failover on Node Failure (P1)
**As a** user, **I want** traffic to automatically reroute when a node fails, **so that** my internet access is uninterrupted.

**Acceptance Criteria:**
- [ ] Master failure: MikroTik removes ECMP route, traffic flows via other masters
- [ ] Endpoint failure: master removes from routing table, balancer redistributes
- [ ] Recovery: node automatically re-added to routing and balancer after health check passes
- [ ] No manual intervention required

### US5: Onboard MikroTik Client (P2)
**As an** operator, **I want** to configure a MikroTik AWG container to connect to the mesh, **so that** my home network routes through the VPN.

**Acceptance Criteria:**
- [ ] `mesh-ctl client prepare --type mikrotik` generates .conf file and .rsc script
- [ ] After container deploy + `mesh-ctl client init`, tunnel is established
- [ ] `mesh-ctl rotate --client` rotates params with sequential ECMP coverage

### US6: Manage Address Space (P2)
**As an** operator, **I want** to define and modify IP ranges for regions, **so that** I can organize the overlay network as it grows.

**Acceptance Criteria:**
- [ ] Ranges defined in CIDR notation in topology.yml
- [ ] `mesh-ctl ip list` shows all allocations with free count
- [ ] `mesh-ctl ip range resize` validates no overlap and no orphaned nodes
- [ ] Balancer IP assignable to any IP (not restricted to first in range)

### US7: Daily Capture Refresh (P3)
**As an** operator, **I want** capture data refreshed daily from popular websites, **so that** generated AWG params use current traffic patterns.

**Acceptance Criteria:**
- [ ] Capture runs on configurable schedule per topology
- [ ] Each master captures independently (different fingerprints)
- [ ] Old captures pruned by retention policy
- [ ] Next rotation uses fresh capture data

## Edge Cases

- **Init token stolen before deploy**: token is time-limited (24h) and single-use. After init, token is invalidated and mTLS takes over.
- **Master offline during Tier 2 rotation**: mesh-ctl checks ALL masters healthy before starting. If any unreachable → abort. Prevents one-sided param change.
- **All masters down simultaneously**: MikroTik has no routes, traffic falls through to ISP default (no VPN). Data plane gracefully degrades.
- **Endpoint at MTU limit**: S3/S4 params validated against computed per-hop MTU. If generated params would exceed MTU → regenerate with tighter constraints.
- **Overlay /24 exhausted (254 nodes)**: topology supports changing overlay space to /16 but requires re-init of all nodes. Documented as disruptive migration.
- **MikroTik container restart during rotation**: ECMP across ≥2 masters ensures zero user-visible downtime. Rotation requires min 2 healthy masters before proceeding.
- **Capture fails for domain**: skip failed domains, log warning, continue with available data. Minimum 1 successful capture required for param generation.
- **gRPC connection interrupted during init**: MESH_TOKEN not invalidated until init fully completes. Re-run `mesh-ctl init` is idempotent.

## Out of Scope

- **Web UI** — mesh-ctl CLI only. No dashboard, no browser-based management.
- **Dynamic mesh discovery** — no auto-discovery of nodes. All topology changes via mesh-ctl.
- **Inter-master communication** — masters are independent. No gossip, no consensus, no sync.
- **End-user VPN client apps** — this is infrastructure transport, not consumer VPN. No mobile apps.
- **Multi-tenant isolation** — single operator, single mesh. No user/org separation.
- **IPv6 overlay** — overlay is IPv4 only. IPv6 transport supported but not overlay addressing.
- **NetBird integration** — deferred to future phase (Phase 3 in amneziawg-scripts roadmap).
- **Kubernetes deployment** — Docker only for now. K8s manifests are future work.

## Dependencies

- **amneziawg-go**: WireGuard-Go fork with AWG obfuscation (github.com/amnezia-vpn/amneziawg-go)
- **Jipok/wgctrl-go**: Go library for WireGuard UAPI access including AWG params
- **gopacket**: Go packet capture library (replaces Python scapy for TLS/QUIC capture)
- **Docker**: container runtime on all hosts
- **MikroTik RouterOS**: client deployment target (container runtime)
- **amneziawg-scripts**: existing Python CLI — AWG param generation logic ported to Go

## Success Criteria

- [ ] Current 5 MikroTik AWG containers replaced with 2 master connections
- [ ] Adding a new endpoint node requires zero MikroTik config changes
- [ ] Tier 1 rotation runs daily with zero user-visible downtime
- [ ] Region failover activates within 30 seconds of node failure
- [ ] Full mesh deployment reproducible from topology.yml alone
- [ ] mesh-ctl can rebuild entire mesh from topology + stored keys

## Open Questions

*All open questions resolved.*

## Clarifications

| # | Category | Question | Resolution | Date |
|---|----------|----------|------------|------|
| C1 | Security | Where to store keys? | WG private keys live ONLY on nodes (/config/). mesh-ctl generates at init, transmits, does not retain. CA key stored at ~/.mesh-ctl/ca.key (0600). No encrypted key store, no age, no git-crypt. Node lost = re-init with new keygen. | 2026-03-26 |
| C2 | Security | Auth model: one token or two? | Single MESH_TOKEN per node. Set at deploy, stored on node and at ~/.mesh-ctl/nodes/<name>/token. gRPC dual auth: mTLS primary, token fallback (always parallel). Used for: init, re-init, CA rotation, disaster recovery. Rotatable via `mesh-ctl token rotate --node X`. Both auth methods lost = SSH bootstrap. | 2026-03-26 |
| C3 | Constraints | Thin mode for MikroTik? | No thin mode. MikroTik container runtime is full Docker — same awg-mesh-node binary, gRPC + mTLS + UAPI all work. mesh-ctl generates RouterOS `/container add` commands instead of docker-compose.yml. Single binary, single image for all platforms. | 2026-03-26 |
| C4 | Observability | Prometheus: built-in or sidecar? | Built-in. Go promhttp on :9091/metrics. Metrics: tunnel_up/down, bytes_tx/rx, healthcheck_status, rotation_last_success. Phase 2 — does not block Phase 1. | 2026-03-26 |
