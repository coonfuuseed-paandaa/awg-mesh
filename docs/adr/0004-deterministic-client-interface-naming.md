# ADR-0004: Deterministic Client Interface Naming

## Status

Accepted (v1.7.0)

## Context

Before v1.7.0, each client tunnel received a WireGuard interface whose name came from a monotonic counter held in `clientPlatformState.nextIdx`: `wg-c0`, `wg-c1`, `wg-c2`, and so on. The counter advanced on every `AddPeer` and never decremented on `RemovePeer`.

This caused three operator-visible problems:

1. **Interface names drifted across restarts.** After `RemovePeer` + `AddPeer` cycles, the same peer could end up under `wg-c5` on one node and `wg-c0` on another. Prometheus scrapers and Grafana dashboards that keyed on interface names saw stale metrics and broken alert queries.
2. **Stale interfaces piled up.** An ungraceful shutdown could leave a `wg-c3` in the kernel. Next reconcile would allocate `wg-c4` for the same peer, leaking the old interface until reboot.
3. **No correlation between interface name and topology.** An operator running `ip link show | grep wg-c` got no signal about *which* peer the interface tunneled to without cross-referencing `wg show` output.

## Decision Drivers

- Interface names must be stable across restarts.
- A peer that is removed and re-added must get the same name.
- External monitoring (Prometheus scrape, log alerting) must not rebuild its series index on every restart.
- Name must fit the Linux interface-name limit (`IFNAMSIZ` = 16 bytes including NUL terminator).
- Collisions must be handled, even if practically improbable.

## Decision

Client interface names are derived deterministically from the peer's WireGuard public key:

```
name = "wg-c" + hex(sha256(peer_pubkey)[:2])
```

- SHA-256 of the peer's 32-byte public key is taken; the first 2 bytes are rendered as 4 lowercase hex characters.
- Final name: `wg-c` + 4 hex chars = 8 characters total. Fits `IFNAMSIZ` with room.
- Name space: 16 bits = 65 536 possible names.
- Mapping is deterministic: same pubkey → same name, across all restarts and all nodes.

### Collision handling

For two distinct pubkeys that hash to the same 4-hex prefix, a numeric suffix is appended: `wg-cNNNN-1`, `wg-cNNNN-2`, etc. Collision is resolved at `uniqueClientIfaceName` call time by:

1. Looking up the pubkey in `byKey` — if the peer is already tracked, reuse its existing name (idempotent re-add).
2. Otherwise, iterating: check in-memory `links` AND call `netlink.LinkByName(name)` to detect kernel-level conflicts from prior crashes. If name is held by a different pubkey, try `name-1`, `name-2`, ... until free.

Birthday-paradox probability at 10 peers in a 16-bit space: ≈0.07%. At 100 peers: ≈7%. Suffix fallback is a second-order safety, not a common path.

### Legacy interface cleanup

After the reconcile loop brings up all expected interfaces, the node scans all kernel interfaces matching `wg-c*`. Any interface whose name does not match `clientIfaceName(pubkey)` for any known peer is deleted via `netlink.LinkDel`, with an INFO log `event=legacy_iface_cleanup, iface=<name>`. This runs on every reconcile and is idempotent — on fresh nodes it finds nothing to remove.

## Alternatives Considered

**Keep monotonic counter, persist it to disk.** Rejected. Counter would still be node-local. Cross-node correlation still impossible. Stale-interface leak still present on crash.

**Free-list of released indices.** Rejected. Nondeterministic across restarts. Still node-local.

**Use full SHA-256 hex (64 chars) as name.** Rejected. `IFNAMSIZ = 16` limits interface names to 15 chars + NUL. Even truncated to 13 hex chars still leaves no room for the `wg-c` prefix.

**Use topology name (`wg-c-master-01`).** Rejected. Topology names can include characters invalid in interface names and can change between runs. Pubkey is the stable identity.

## Consequences

### Positive

- Prometheus/Grafana queries by interface name are stable.
- Operator running `ip link show` can correlate interfaces to peers via `wg show <iface>` + pubkey lookup.
- Stale interfaces are cleaned up automatically on every reconcile.
- RemovePeer + AddPeer of the same peer does not churn the interface name.

### Negative

- **Breaking change for monitoring scrapers.** Any regex matching `wg-c[0-9]+$` must be updated to `wg-c[0-9a-f]{4}(-[0-9]+)?$`.
- `IFNAMSIZ` limits name to 4 hex chars after the prefix — cannot trivially extend to more entropy without restructuring the prefix.
- Birthday-paradox collision risk grows with peer count; unlikely in practice for <100 peers per client, but would require suffix fallback at larger scale.

### Neutral

- First reconcile after v1.7.0 upgrade emits INFO `event=legacy_iface_cleanup` for each pre-migration `wg-cN`. Expected behavior.

## References

- Implementation: `pkg/node/client_linux.go` — `clientIfaceName`, `uniqueClientIfaceName`, `clientIfaceNameConflicts`, legacy cleanup in `reconcileFromTransportState`.
- Spec: `.agent/specs/client-ecmp/spec.md` FR-8, Clarification C4.
- PR #31.
