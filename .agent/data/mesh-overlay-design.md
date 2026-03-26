# AWG Mesh Overlay Network Design

**Status:** Draft
**Created:** 2026-03-26

## Problem

Current routing: 1 node = 1 MikroTik container + veth + routing table. Rigid, doesn't scale, relay is hacky.

## Design Principles

1. AWG containers = transport only (like cables)
2. Routing = to IPs, not to containers
3. Default path: ISP → RU relay → endpoint (faster than direct)
4. Easy extensibility: new node = peer on RU relays, done

## Architecture Layers

```
┌─────────────────────────────────────────────┐
│ POLICY:  address-list → route to region IP  │  MikroTik
├─────────────────────────────────────────────┤
│ TRANSPORT: MikroTik ↔ RU relays (2 tunnel) │  2 AWG containers
├─────────────────────────────────────────────┤
│ MESH: RU relays ↔ all endpoints            │  RU nodes
├─────────────────────────────────────────────┤
│ ENDPOINTS: KZ-01,02,03 / PL / US / ...     │  AWG nodes
└─────────────────────────────────────────────┘
```

## Address Spaces

| Layer | Space | Purpose |
|-------|-------|---------|
| Tunnel P2P | 10.8.x.0/24 | AWG handshake, point-to-point |
| Mesh overlay | 172.20.70.0/24 | Node identity, routing next-hop |
| Docker | 172.20.{39-45}.0/24 | Containers on nodes |

## Overlay Mesh Space (172.20.70.0/24)

```
.1   = MikroTik (gateway)
.1x  = RU transit nodes
  .11 = ru-01 (141.98.191.38)
  .12 = ru-02 (147.45.185.141)
.2x  = KZ endpoint nodes
  .21 = kz-01 (176.12.75.213)
  .22 = kz-02 (38.180.37.82)
  .23 = kz-03 (176.100.42.175)
.3x  = PL endpoint nodes
  .31 = pl-01 (37.252.11.125)
.4x  = US endpoint nodes
  .41 = us-01 (103.113.70.106)
.5x  = reserved
```

## Routing

### MikroTik

```
# Reach overlay via RU relays
172.20.70.0/24 gateway=<RU-01-tunnel> distance=1
172.20.70.0/24 gateway=<RU-02-tunnel> distance=2

# Policy routing — KZ with ECMP + failover
0.0.0.0/0 gateway=172.20.70.21 table=vpn-route-kz distance=1 check-gateway=ping
0.0.0.0/0 gateway=172.20.70.22 table=vpn-route-kz distance=1
0.0.0.0/0 gateway=172.20.70.23 table=vpn-route-kz distance=2  # backup

# PL
0.0.0.0/0 gateway=172.20.70.31 table=vpn-route-pl distance=1 check-gateway=ping

# US
0.0.0.0/0 gateway=172.20.70.41 table=vpn-route-us distance=1 check-gateway=ping
```

### RU Relays

```
# Each RU relay knows how to reach endpoints via AWG tunnels
172.20.70.21/32 via <awg-tunnel-to-kz-01>
172.20.70.22/32 via <awg-tunnel-to-kz-02>
172.20.70.23/32 via <awg-tunnel-to-kz-03>
172.20.70.31/32 via <awg-tunnel-to-pl-01>
172.20.70.41/32 via <awg-tunnel-to-us-01>
```

### Endpoint Nodes

```
# Assign overlay IP on loopback
ip addr add 172.20.70.XX/32 dev lo

# Default route back through RU relay (for return traffic)
```

## Benefits vs Current

| Aspect | Current | New |
|--------|---------|-----|
| Add node | Container + veth + routing table on MikroTik | Peer on RU relay only |
| Failover | None (single path) | check-gateway + distance |
| ECMP | Impossible | Native (same distance routes) |
| MikroTik containers | 5 (one per node) | 2 (one per RU relay) |
| Relay | Hacky docker networking | Standard IP routing |
| Node visibility | MikroTik must know every node | MikroTik knows only RU relays |

## Implementation (per node)

1. Assign overlay IP: `ip addr add 172.20.70.XX/32 dev lo`
2. Add to docker-compose .env: `OVERLAY_IP=172.20.70.XX`
3. Peer with RU-01 and RU-02 (AWG tunnels)
4. RU relays add static route: `172.20.70.XX/32 via <tunnel>`
5. MikroTik: add gateway to appropriate routing table

## Open Questions

- How to implement relay routing cleanly on RU nodes? (current: hacks)
- Router service inside Docker? Or host-level routing?
- NAT on relay or on endpoint?
- Return path: symmetric via same relay or asymmetric?
