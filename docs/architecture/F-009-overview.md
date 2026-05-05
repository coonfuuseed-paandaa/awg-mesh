# F-009 Architecture Overview — awg-mesh v2.0

**Status:** v2.0.0-alpha.1 (CR-001 foundation landed; daemon implementation in CR-002+)
**Spec:** see project repo `.agent/specs/F-009-mesh-architecture/spec.md` (local development)
**Genesis:** `.agent/genesis/F-009-mesh-redesign-vision.md`
**Last updated:** 2026-05-01

This is the operator-facing one-pager for the v2.0 architecture. Skim it before
deploying; consult the full spec for FR/NFR detail.

## What changed vs v1.x

awg-mesh v1.x was a hub-spoke encrypted relay with **per-peer interfaces** on every
node and an **ECMP multipath route** for overlay traffic. Three problems:

1. **Multipath src quirk** — Linux kernel's `inet_select_addr` returned the chosen
   nexthop interface's primary address (transport `/30`) instead of the route-level
   `prefsrc` (overlay `/32`), causing source-IP leak verified via tcpdump.
2. **Two-layer addressing** — operator had to reason about `transport_pool` AND
   `overlay_supernet` simultaneously; topology files duplicated the layering.
3. **Per-pair anti-DPI rotation** — operationally complex N×M coordination.

v2.0 (F-009) collapses to a **flat AmneziaWG mesh with role-tagged nodes**.

## High-level architecture

```text
                  control plane (mesh-ctl daemon)
                ↑   ↑   ↑   ↑   ↑
                │   │   │   │   │ signal/management (gRPC + mTLS)
   ┌───────┐ ┌──┴───┴───┐ ┌──┴──────────┐ ┌──────────┐
   │client │─│  master  │─│   egress    │─│ internet │
   │  /32  │ │   /32    │ │   /32       │ │          │
   │       │ │ (decrypt+│ │ (MASQUERADE │ └──────────┘
   │ vanilla│ │  forward)│ │  at iface) │
   │  WG   │ │          │ │            │
   └───────┘ └──────────┘ └────────────┘
                  ↕                       ←── AmneziaWG mesh-internal
              ┌───────┐                       (anti-DPI rotated mesh-wide)
              │ingress│←── public internet
              │       │    (TLS SNI passthrough OR terminate)
              └───────┘
```

## Roles

Every node in v2.0 carries one or more **roles** declared in `mesh-topology.yml`:

| Role | Function |
|------|----------|
| `client` | End-user device (Mikrotik, home server). Initiates outbound vanilla-WG to a master. EXCLUSIVE — cannot combine with other roles. |
| `master` | Accepts client tunnels (vanilla-WG listener) and routes to mesh-internal AmneziaWG channel. |
| `egress` | Internet-bound interface masquerades outbound traffic. The ONLY place NAT exists in v2.0. |
| `ingress` | Public-IP listener accepting inbound HTTP/HTTPS/TCP/UDP, forwarding to mesh client services. Cloudflared analogue. |
| `balancer` | Policy engine selecting egress per flow (dumb / labeled / smart-future). |

`client` is exclusive; `master`, `balancer`, `egress`, `ingress` are composable on
the same host. Small deployments collapse all four onto one VPS; large deployments
split them.

## Key invariants

- **Single AWG TUN per non-master node.** Master alone has two devices: vanilla-WG
  (port 51820, client-facing) and AmneziaWG (port 51821, mesh-internal).
- **NO NAT in mesh interior.** Egress masquerades only at internet boundary.
- **Hub-spoke.** Client peers ONLY with master(s). No direct client↔egress.
- **Each entity has a unique overlay /32.** All nodes addressable by overlay address.
- **HA-2 partitioned ownership.** Client peers with all masters; AllowedIPs
  partitioned per master with no overlap; control plane reassigns ownership on
  failover.
- **Federated masters.** Masters peer with each other through mesh-internal AWG.

## Anti-DPI rotation

Existing three-tier scheme preserved; what changes is **mesh-wide atomic propagation**:

| Tier | Default cadence | What rotates |
|------|-----------------|--------------|
| tier1 | 24h | Jc/Jmin/Jmax + I1–I5 (regenerated from `domains.txt` capture) |
| tier2 | 168h | S1/S2/H1–H4 |
| tier3 | 720h | WG keypair |

`mesh-ctl rotate --tier N` triggers immediate rotation; control plane orchestrates
simultaneous apply across all mesh-internal nodes within ~30 seconds of trigger.
Vanilla-WG client links are excluded (vanilla WG has no obfuscation params).

## Topology schema (v2.0)

```yaml
schema_version: 2

mesh:
  name: my-mesh
  overlay_supernet: 172.21.92.0/24

nodes:
  - name: master-01
    roles: [master, balancer, egress]
    overlay_ip: 172.21.92.2
    public_ip: 203.0.113.10
    internet_iface: eth0
  - name: home-01
    roles: [client]
    overlay_ip: 172.21.92.130
    preferred_master: master-01

services:
  - name: jellyfin
    owner_node: home-01
    protocol: tcp
    local_port: 8096
    ingress:
      - hostname: media.example.com
        mode: sni_passthrough
        ingress_node: ingress-01

rotation:
  tier1_interval: 24h
  tier2_interval: 168h
  tier3_interval: 720h
  preset: aggressive
  adaptive: true

observability:
  audit_retention_days: 90
  cert_rotation_days: 90
  prometheus_listen: ":9091"
```

## Supported platforms

- **Linux kernel:** ≥4.19 with WireGuard kernel module (built-in since 5.6) for
  every mesh node. Mesh-internal nodes additionally require AmneziaWG kernel
  module ≥1.0 (build from `amneziawg-linux-kernel-module` repo).
- **MikroTik RouterOS:** the v2.0 runtime release gate targets RouterOS 7.21+
  container deployments running `awg-mesh-client`, because the current client
  data plane requires container-side nftables support. This is not the same as
  the generator syntax floor: `mesh-ctl` still emits version-specific
  `/container` syntax for the documented legacy/transitional pivots
  (`7.16.2`, `7.20.8`, `7.21.4`) so script generation remains regression
  tested across those dialects. Native RouterOS vanilla WireGuard compatibility
  remains a required future track, but it is not wired into the current release
  gate.
- **macOS / Windows / FreeBSD:** OUT of scope for v2.0 (AWG kernel module Linux-only).

## CR roadmap

| CR | Scope | Target release |
|----|-------|----------------|
| CR-001 | Foundation: skeleton + v1.x removal (this release) | v2.0.0-alpha.1 |
| CR-002 | Control plane daemon (identity, peer-list, ledger, rotation, NAT signal) | v2.0.0-alpha.2 |
| CR-003 | clientd self-config agent | v2.0.0-alpha.2 |
| CR-004 | Master protocol bridge (vanilla-WG + AmneziaWG dual listener) | v2.0.0-alpha.3 |
| CR-005 | Egress mode (MASQUERADE) | v2.0.0-alpha.3 |
| CR-006 | Ingress mode (SNI passthrough + TLS terminate + ACME + HTTP/3 + WebSocket + UDP) | v2.0.0-alpha.4 |
| CR-007 | Balancer policy engine (dumb / labeled) | v2.0.0-alpha.4 |
| CR-008 | Mesh-wide rotation orchestration + adaptive trigger | v2.0.0-beta.1 |
| CR-010 | mesh-ctl redesign | v2.0.0-beta.1 |
| CR-011 | Critical suite v2 (full implementation of 18+ tests) | v2.0.0-beta.2 |
| CR-012 | Emulation playbook v2 | v2.0.0-rc.1 |
| CR-013 | Migration tooling v1.x → v2.0 | v2.0.0-rc.1 |
| CR-014 | MikroTik native vanilla-WG bridge (deferred/unwired); current gate uses RouterOS container deployment | future |
| Release | Tag, push, image publication, retag verification | v2.0.0 |

See plan F-009 for full CR dependency graph and parallelism map.
