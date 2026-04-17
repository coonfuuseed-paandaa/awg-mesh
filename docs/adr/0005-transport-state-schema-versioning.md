# ADR-0005: TunnelTransport Schema Versioning

## Status

Accepted (v1.7.0)

## Context

Before v1.7.0, `pkg/transport.NodeTransportState` had no schema version. Fields were added to `TunnelTransport` over time without a migration story: older state files deserialized cleanly because unknown fields were ignored and missing fields defaulted to zero values. This was fine until the `TunnelTransport` extension for v1.7.0 required two semantic changes that zero-value defaults could not safely represent:

1. **`AllowedIPs []string`** — the peer's AllowedIPs from the originating `AddPeer` RPC. An empty slice cannot be treated as "0.0.0.0/0" in v1.7.0+ because that is exactly the H4 bug we are fixing — silently installing a catch-all route when the operator's intent was a narrow overlay CIDR.
2. **`PersistentKeepalive int32`** — WG keepalive seconds. `0` has two valid meanings: "disabled" (don't send keepalive) and "unset" (fall back to default 25 s).

Without a schema version, the reconcile path could not tell "a v1.5.0 state file where these fields were simply not persisted" apart from "a v1.7.0 state file with explicit empty values". Same bytes on disk, different required behavior.

## Decision Drivers

- Fix the H4 bug (hardcoded `allowedIPs=["0.0.0.0/0"]` in reconcile) without breaking existing nodes on upgrade.
- Operators must not need to manually regenerate state files.
- Future schema changes need a stable extension point.
- Migration must be durable: the fallback WARN log fires once, not on every boot.

## Decision

Introduce `schema_version: int` at the top level of `NodeTransportState`:

```yaml
schema_version: 1           # added in v1.7.0
overlay_ip: 172.20.70.130
tunnels:
  - name: wg-cXXXX
    transport_ip: 10.255.0.2
    peer_transport_ip: 10.255.0.1
    peer_public_key: <hex>
    peer_endpoint: master-01.example:51820
    balancer_ip: 172.20.70.1
    allowed_ips: ["172.20.70.0/24"]
    persistent_keepalive: 25
```

### Contract

- **`CurrentSchemaVersion = 1`** is defined in `pkg/transport`.
- Every write stamps `state.SchemaVersion = CurrentSchemaVersion` — the node always persists the current schema.
- `IsLegacySchema(state) bool` returns `true` when `state.SchemaVersion == 0` (absent field deserializes to zero).
- `ApplyLegacyDefaults(state, logger)` fills `AllowedIPs` with `["0.0.0.0/0"]` and `PersistentKeepalive` with `25` for each tunnel that has empty values, logs exactly ONE `event=transport_state_legacy_schema, tunnel_count=N` WARN, and sets `state.SchemaVersion = CurrentSchemaVersion`.
- On reconcile, if legacy schema is detected, the migrated state is persisted to disk immediately — so the WARN does not fire again on next boot.
- In v1.7.0+ schema (`schema_version: 1`), an empty `allowed_ips` for any tunnel is a hard error: reconcile refuses to bring up a tunnel whose AllowedIPs semantics are ambiguous. The operator gets a clear error, not a silently catch-all route.

### RPC boundary enforcement

`AddPeer` gRPC handler (`pkg/grpc/handlers.go`) rejects requests with empty `allowed_ips` before persisting — failure happens at the RPC boundary, not at the next reconcile. This prevents a buggy `mesh-ctl` from writing a v1.7.0 state file that the node would later hard-error on.

## Alternatives Considered

**Add version to every tunnel instead of top-level.** Rejected. Tunnels within one state file always share the same schema — per-tunnel versioning would add fields for no gain.

**Sniff the presence of `allowed_ips` as an implicit version marker.** Rejected. Works accidentally for this schema jump but does not scale: future fields could be legitimately absent without meaning "pre-v1.7.0".

**Force operators to re-run `mesh-ctl client init` on upgrade.** Rejected. Clients are often unattended (home routers, remote boxes). Hard break on upgrade is an ops burden. Auto-migrate with one WARN is the operator-friendly path.

**Use a string version tag (`"v1"`, `"v2"`).** Rejected. Integer is more compact, easier to compare, and the `omitempty` YAML tag skips it when zero — legacy files still deserialize cleanly without explicit version handling.

## Consequences

### Positive

- **H4 closed for v1.7.0 schema** — reconcile now uses topology-driven `AllowedIPs` verbatim, no hardcoded fallback.
- **Pre-v1.6.0 state migrates transparently** — one WARN, then durable state rewrite. Operators see the migration in logs, can verify after first boot.
- **Future schema changes have a clear contract** — bump `CurrentSchemaVersion` to 2, add migration step for `state.SchemaVersion == 1`, ship.
- **RPC boundary rejection** prevents `mesh-ctl` bugs from landing invalid state on the node.

### Negative

- Every reconcile pays the cost of one extra check (`IsLegacySchema`) — negligible, O(1) integer compare.
- Migration is WARN-noisy for the first boot after upgrade. Intentional — the WARN is the migration receipt.

### Neutral

- The `schema_version` field uses `omitempty` in the YAML tag, so legacy state files (without it) still deserialize cleanly into `SchemaVersion: 0` — no parser changes needed.

## References

- Implementation: `pkg/transport/node_state.go` — `SchemaVersion`, `CurrentSchemaVersion`, `IsLegacySchema`, `ApplyLegacyDefaults`.
- Reconcile path: `pkg/node/client_linux.go` — `reconcileFromTransportState` with schema-aware fallback.
- RPC validation: `pkg/grpc/handlers.go` — `saveNodeTransportStateAfterPeerAdded` rejects empty `allowed_ips`.
- Spec: `.agent/specs/client-ecmp/spec.md` FR-4, FR-5, Clarification C1.
- Closes finding H4 (issue #22).
- PR #27.
