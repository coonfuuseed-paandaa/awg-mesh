# AWG Mesh — Project Constitution

**Created:** 2026-03-26
**Status:** Ratified

These principles are NON-NEGOTIABLE. Every design decision, implementation choice,
and code change MUST comply. Violations are CRITICAL findings.

---

## C1: Control Plane / Data Plane Separation

**mesh-ctl** is the control plane. **awg-mesh-node** is the data plane.

- mesh-ctl: stateless CLI on admin PC. Runs on demand. Manages topology, rotation, onboarding.
- awg-mesh-node: stateful process on each host. Runs continuously. Manages tunnels, routes, health.
- Data plane operates independently when control plane is unavailable.
- Control plane NEVER runs on mesh nodes. Data plane NEVER runs on admin PC.

## C2: Masters Are Independent

Master nodes MUST NOT communicate with or depend on each other.

- No gossip protocol, no consensus, no shared state between masters.
- Each master is a standalone unit — unaware of other masters.
- MikroTik handles transport-level failover via ECMP.
- Adding or removing a master MUST NOT affect other masters' operation.

## C3: Topology as Single Source of Truth

`mesh-topology.yml` is the ONLY authoritative description of the mesh.

- All node identities, IP allocations, ranges, schedules — defined in topology.
- mesh-ctl reads topology to determine what actions to take.
- Nodes store their own runtime state locally (/config/) but topology defines intent.
- Manual changes to node configs that contradict topology WILL be overwritten.

## C4: UAPI-First Configuration

Runtime configuration changes MUST use UAPI (WireGuard userspace API), not file replacement + restart.

- AWG parameter changes: UAPI SET via device.IpcSet() or Jipok/wgctrl-go.
- Zero downtime for Tier 1 and Tier 2 rotation.
- Config files are for persistence (survive restart), not for runtime changes.
- Restart is LAST RESORT, not default mechanism.

## C5: mTLS Mandatory, No Plaintext Management

All management communication MUST use mutual TLS or authenticated token.

- gRPC: mTLS as primary, MESH_TOKEN as fallback. Both always available.
- No plaintext HTTP, no unauthenticated endpoints, no API keys in URLs.
- SSH only for initial bootstrap (installing Docker on bare host).
- MESH_TOKEN is the emergency access method when mTLS fails.

## C6: No External Dependencies

The mesh MUST operate with ZERO external infrastructure dependencies.

- No etcd, no Consul, no ZooKeeper, no Kubernetes.
- No cloud services (no AWS KMS, no Vault, no external CA).
- No database servers (state is local files on each node).
- Dependencies: Docker runtime + the binary itself. Nothing else.

## C7: Single Language, Single Binary

All components MUST be written in Go. Each deployable is a single static binary.

- awg-mesh-node: one binary, all modes (master/endpoint/client).
- mesh-ctl: one binary, all commands.
- No Python, no Node.js, no shell scripts in production paths.
- amneziawg-go used as Go library (import), not as external process.
- Static linking, no CGO, runs on any Linux (Alpine, scratch).
