# AWG Mesh: Address Space Management

**Part of:** mesh-router-design.md
**Created:** 2026-03-26

## Problem

Hardcoded `.X0` = balancer, `.X1-.X9` = nodes works for ≤9 nodes per region.
Need: named ranges, editable, support for growth beyond 9.

## Design: Named Ranges in topology.yml

Ranges use standard CIDR notation or explicit start-end.
Balancer IP is explicit — any IP, doesn't have to be first in range.

```yaml
# mesh-topology.yml
overlay:
  space: 172.20.70.0/24

  ranges:
    - name: gateway
      cidr: 172.20.70.1/32
      description: "MikroTik gateway"

    - name: masters
      cidr: 172.20.70.8/29          # .8-.15 (8 IPs)
      description: "Master/ingress nodes"
      balancer: 172.20.70.8         # any IP in range

    - name: kz
      cidr: 172.20.70.16/28         # .16-.31 (16 IPs)
      description: "Kazakhstan endpoints"
      balancer: 172.20.70.16

    - name: pl
      cidr: 172.20.70.32/29         # .32-.39 (8 IPs)
      description: "Poland endpoints"
      balancer: 172.20.70.32

    - name: us
      cidr: 172.20.70.40/29         # .40-.47 (8 IPs)
      description: "United States endpoints"
      balancer: 172.20.70.40

    - name: reserved
      range: 172.20.70.48-172.20.70.99   # explicit range (when CIDR doesn't fit)
      description: "Reserved for future regions"

    # 100-254 = unallocated
```

### Range notation

Two formats supported:

```yaml
# CIDR (preferred — standard, unambiguous):
cidr: 172.20.70.16/28     # 172.20.70.16 - 172.20.70.31

# Explicit range (when CIDR boundaries don't align):
range: 172.20.70.48-172.20.70.99
```

### Balancer IP

**Any valid IP.** Does not have to be first in range, does not have to be in the range at all (though recommended).

```yaml
# Balancer at start of range (typical):
balancer: 172.20.70.16

# Balancer at dedicated IP outside node range:
balancer: 172.20.70.100    # separate from node IPs

# No balancer (pinned routing only):
# omit balancer field
```

## How It Works

### Assigning IPs

```bash
# Auto-assign from range:
mesh-ctl endpoint prepare --name kz-04 --region kz
# → picks next free IP in "kz" range: 172.20.70.24

# Explicit assignment:
mesh-ctl endpoint prepare --name kz-04 --overlay-ip 172.20.70.25
# → validates IP is within correct range and not taken

# Show allocations:
mesh-ctl ip list
```

```
OVERLAY ADDRESS SPACE: 172.20.70.0/24

Range: gateway (1-1)
  .1    mikrotik-home          gateway

Range: masters (10-19, balancer: .10)
  .10   [balancer]             all masters
  .11   master-01              141.98.191.38
  .12   master-02              147.45.185.141
  .13   -                      (free)
  ...
  .19   -                      (free)
  Available: 7/9

Range: kz (20-39, balancer: .20)
  .20   [balancer]             ECMP: .21,.22,.23
  .21   kz-01                  176.12.75.213
  .22   kz-02                  38.180.37.82
  .23   kz-03                  176.100.42.175
  .24   -                      (free)
  ...
  .39   -                      (free)
  Available: 16/19

Range: pl (40-49, balancer: .40)
  .40   [balancer]             ECMP: .41
  .41   pl-01                  37.252.11.125
  Available: 8/9

Range: us (50-59, balancer: .50)
  .50   [balancer]             ECMP: .51
  .51   us-01                  103.113.70.106
  Available: 8/9

Range: reserved (60-99)
  Available: 40/40

Unallocated: 100-254 (155 IPs)
Total used: 8/254
```

### Editing Ranges

```bash
# Resize:
mesh-ctl ip range resize --name kz --cidr 172.20.70.16/27
# → validates no overlap, no orphaned nodes

# Add new range:
mesh-ctl ip range add --name de --cidr 172.20.70.64/29 \
  --description "Germany endpoints" --balancer 172.20.70.64

# Move range (reassigns all nodes — disruptive):
mesh-ctl ip range move --name us --cidr 172.20.70.80/29 --force

# Set/change balancer:
mesh-ctl ip range set-balancer --name kz --ip 172.20.70.30
# → balancer doesn't have to be first IP

# Rename:
mesh-ctl ip range rename --name reserved --new-name eu

# Delete (must be empty):
mesh-ctl ip range delete --name reserved
```

### Balancer IP

Any IP (explicit). Automatically maintained by master nodes:

```
Range "kz": 172.20.70.16/28, balancer=172.20.70.16
  Nodes: .17 (kz-01), .18 (kz-02), .19 (kz-03)

  Master routing table:
    172.20.70.16/32 nexthop via wg-kz-01 weight 100
                    nexthop via wg-kz-02 weight 100
                    nexthop via wg-kz-03 weight 100
```

Balancer pool auto-updated when nodes are added/removed from the range.

### Validation Rules

1. Ranges MUST NOT overlap
2. Balancer IP recommended within its range (but not enforced)
3. Node IP MUST be within its declared range
4. Range resize MUST NOT orphan existing nodes
5. .0 and .255 reserved (network/broadcast in /24)
6. Total space: /24 = 254 usable IPs (enough for foreseeable future)
7. If /24 ever not enough → overlay can be changed to /16 (requires re-init)

### Topology File Integration

```yaml
# Full topology.yml example:
overlay:
  space: 172.20.70.0/24
  physical_mtu: 1500
  awg_overhead: 60

  ranges:
    - name: gateway
      cidr: 172.20.70.1/32
    - name: masters
      cidr: 172.20.70.8/29
      balancer: 172.20.70.8
    - name: kz
      cidr: 172.20.70.16/28
      balancer: 172.20.70.16
    - name: pl
      cidr: 172.20.70.32/29
      balancer: 172.20.70.32
    - name: us
      cidr: 172.20.70.40/29
      balancer: 172.20.70.40

masters:
  - name: master-01
    host: 141.98.191.38
    overlay-ip: 172.20.70.9      # auto-assigned from "masters" range (.8/29 = .8-.15)

  - name: master-02
    host: 147.45.185.141
    overlay-ip: 172.20.70.10

endpoints:
  - name: kz-01
    host: 176.12.75.213
    port: 853
    overlay-ip: 172.20.70.17     # auto-assigned from "kz" range (.16/28 = .16-.31)
    region: kz                    # links to range name

  - name: kz-02
    host: 38.180.37.82
    port: 853
    overlay-ip: 172.20.70.18
    region: kz

  - name: kz-03
    host: 176.100.42.175
    port: 853
    overlay-ip: 172.20.70.19
    region: kz

  - name: pl-01
    host: 37.252.11.125
    port: 853
    overlay-ip: 172.20.70.33     # from "pl" range (.32/29 = .32-.39)
    region: pl

  - name: us-01
    host: 103.113.70.106
    port: 853
    overlay-ip: 172.20.70.41     # from "us" range (.40/29 = .40-.47)
    region: us

clients:
  - name: mikrotik-home
    type: mikrotik
    overlay-ip: 172.20.70.1
    masters: [master-01, master-02]

capture:
  domains_file: domains.txt
  schedule: "0 3 * * *"
  retention_days: 7

rotation:
  defaults:
    tier1_interval: 24h
    tier2_interval: 7d
    tier3_interval: 30d
    preset: aggressive
```

## MTU: Automatic Calculation + TCP Clamp-to-PMTU

### Problem

Each hop of AWG encapsulation eats MTU. Path:

```
Client (1500) → MikroTik → AWG tunnel → Master → AWG tunnel → Endpoint → Internet
                            -60 overhead              -60 overhead
```

Two AWG hops = 120 bytes overhead. If both tunnels use MTU 1420, inner tunnel gets 1420-60=1360.
If endpoint sets MTU 1420 but master tunnel already reduced to 1420, effective = 1360 — fragmentation.

### Solution: automatic per-hop MTU calculation

```
Physical MTU:     1500
AWG overhead:     60 bytes (IP 20 + UDP 8 + WG 16 + AWG obfuscation ~16)
                  Note: exact overhead depends on S3/S4 params

Hop 0 (ISP):     MTU = 1500
Hop 1 (client → master):    MTU = 1500 - 60 = 1440
Hop 2 (master → endpoint):  MTU = 1440 - 60 = 1380
```

### In topology.yml

```yaml
overlay:
  physical_mtu: 1500        # ISP MTU (usually 1500)
  awg_overhead: 60          # WireGuard + AWG encapsulation overhead

  # Auto-calculated per node:
  # master tunnel MTU = physical_mtu - awg_overhead = 1440
  # endpoint tunnel MTU = physical_mtu - 2*awg_overhead = 1380
  # (or computed from actual hop count)
```

awg-mesh-node computes its interface MTU automatically based on hop count:

```
endpoint (1 hop from internet):   wg MTU = physical_mtu - awg_overhead
master (2 hops — client→master→endpoint):
  ingress interface MTU = physical_mtu - awg_overhead       (1440)
  egress interface MTU  = physical_mtu - 2*awg_overhead     (1380)
```

### TCP Clamp-to-PMTU

Every AWG interface applies MSS clamping to prevent fragmentation:

```bash
iptables -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
```

awg-mesh-node applies this automatically on all WireGuard interfaces at startup.
Already present in current wg-easy config (`WG_PRE_UP`), will be built into awg-mesh-node.

### S3/S4 constraints (from awg_gen)

AWG obfuscation adds variable overhead via S3 (cookie reply) and S4 (transport):

```
S3 max = physical_mtu - wg_mtu - WG_OVERHEAD(44) - WG_COOKIE_OVERHEAD(48)
S4 max = physical_mtu - wg_mtu - WG_OVERHEAD(44) - 16
```

awg-mesh-node validates generated S3/S4 against actual MTU at each hop.
If params would cause MTU violation → regenerate with adjusted constraints.

### Example: full path MTU

```
Client PC (MSS 1460)
  → MikroTik (no encap)
  → AWG tunnel to master (MTU 1440, MSS clamped to 1400)
    → AWG tunnel to endpoint (MTU 1380, MSS clamped to 1340)
      → Internet (endpoint NATs, responds)

Return: same path, MSS already negotiated at 1340.
No fragmentation anywhere in the chain.
```

### Override per-node

```yaml
endpoints:
  - name: kz-01
    mtu_override: 1400    # manual override (e.g., ISP has lower MTU)
```

### Note on Address Changes

If overlay-ip of a live node changes (range move/resize):
1. mesh-ctl must update ALL nodes that reference this IP
2. Masters update routing tables
3. MikroTik routing tables update
4. This is a disruptive operation — mesh-ctl warns and requires --force
