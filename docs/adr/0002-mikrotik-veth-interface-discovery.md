# ADR-0002: MikroTik VETH Interface Discovery

## Status

Proposed

## Context

MikroTik RouterOS 7.20 (stable 2025-09-29) introduced a breaking change for containers: VETH interfaces inside containers now use the RouterOS-assigned name instead of `eth0`.

| RouterOS | Inside Container | Multiple VETHs |
|----------|------------------|----------------|
| < 7.20 | Always `eth0` | One VETH only |
| >= 7.20 | RouterOS name (e.g. `veth-awg`) | Multiple VETHs supported |

This broke Pi-hole, ZeroTier, and all containers that hardcoded `eth0` in configs or scripts.

Existing AWG containers handle this differently:
- **timbrs/amneziawg-mikrotik** (UDP proxy): binds `0.0.0.0` — doesn't reference interfaces at all
- **wiktorbgu/amneziawg-mikrotik** (awg-quick): user hardcodes VETH name in PostUp/PostDown rules inside `.conf`

Our awg-mesh-node container uses interface names for:
- nftables masquerade (`SetupNAT("eth0")` — currently hardcoded)
- DSCP routing (nftables rules on incoming interface)
- DNS server bind address
- Overlay IP assignment

## Decision Drivers

* Must work on both ROS < 7.20 (eth0) and >= 7.20 (custom names)
* Must support multiple VETHs (ROS 7.20+)
* Should not require user to manually edit container code for each VETH name
* Existing community patterns: first non-loopback, default route interface, env var injection

## Considered Options

### Option 1: Hardcode `eth0`

- **Pros**: Simple, works for < 7.20 and standard Docker
- **Cons**: Breaks on ROS >= 7.20 with custom VETH names

### Option 2: Environment Variable (`MESH_INTERFACE`)

- **Pros**: Explicit, user controls, works everywhere
- **Cons**: Requires user to configure env var, error-prone

### Option 3: Auto-discover via default route

```go
// Find interface with default route
routes, _ := netlink.RouteList(nil, netlink.FAMILY_V4)
for _, r := range routes {
    if r.Dst == nil { // default route
        link, _ := netlink.LinkByIndex(r.LinkIndex)
        return link.Attrs().Name // e.g. "veth-awg" or "eth0"
    }
}
```

- **Pros**: Works on any platform, no configuration needed, handles ROS version difference transparently
- **Cons**: Assumes single default route exists

### Option 4: Auto-discover + env var override (hybrid)

1. Check `MESH_INTERFACE` env var — if set, use it
2. Otherwise, find interface with default route via netlink
3. Log discovered interface name at startup

- **Pros**: Works everywhere, auto for 99% cases, override for edge cases
- **Cons**: Slightly more code

## Decision

**Option 4: Auto-discover + env var override.**

The auto-discovery uses the default route interface (via netlink), which works identically on:
- Docker standard (`eth0`)
- MikroTik ROS < 7.20 (`eth0`)
- MikroTik ROS >= 7.20 (custom VETH name, e.g. `veth-awg`)
- Any Linux environment with any interface naming scheme

The `MESH_INTERFACE` env var provides escape hatch for unusual network topologies.

## Consequences

### Positive

- Zero-config for all common deployments
- Removes hardcoded `eth0` from `setupExitMode()` and future interface references
- Forward-compatible with MikroTik multi-VETH (7.20+)
- Works on non-MikroTik Docker environments too

### Negative

- Auto-discovery may pick wrong interface in multi-network containers
- Mitigation: `MESH_INTERFACE` override + startup log message showing discovered interface

## Implementation

Replace all hardcoded `eth0` references with:

```go
func discoverWANInterface() string {
    if envIface := os.Getenv("MESH_INTERFACE"); envIface != "" {
        return envIface
    }
    routes, err := netlink.RouteList(nil, netlink.FAMILY_V4)
    if err != nil {
        return "eth0" // safe fallback
    }
    for _, r := range routes {
        if r.Dst == nil {
            link, err := netlink.LinkByIndex(r.LinkIndex)
            if err == nil {
                return link.Attrs().Name
            }
        }
    }
    return "eth0" // fallback
}
```

Files to modify:
- `pkg/node/master_linux.go:setupExitMode()` — replace `"eth0"` with `discoverWANInterface()`
- `pkg/node/node.go` or new `pkg/node/interface_linux.go` — add discovery function

## Research Sources

| Source | Finding |
|--------|---------|
| MikroTik 7.20 changelog | "allow to use multiple veths in a container, change the in container interface name to same as in RouterOS" |
| forum.mikrotik.com Pi-hole thread | Confirmed `eth0` → RouterOS VETH name, broke existing configs |
| forum.mikrotik.com ZeroTier thread | Same breakage, migration required |
| timbrs/amneziawg-mikrotik source | UDP proxy, binds 0.0.0.0, no interface detection |
| wiktorbgu/amneziawg-mikrotik | User hardcodes VETH in .conf PostUp/PostDown rules |
| MikroTik container docs | ARM64, ARMv7, x86_64 only. No MIPS. External USB storage required. |

## Related Decisions

- ADR-0001: Multi-Image Docker Strategy — client image must include auto-discovery
- Constitution C6: No external dependencies — discovery uses netlink (already a dependency)
