# Changelog

All notable changes to awg-mesh are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## v1.12.9 — 2026-04-20

### Fixes
- **proto descriptor/struct drift (local tracker #147 layer 4):** `AddTunnelRequest.allowed_ips` (field 13) was present in the Go struct and `.proto` source but absent from `file_types_proto_rawDesc` — the raw binary descriptor blob that `google.golang.org/protobuf` uses as authoritative for wire marshal/unmarshal. As a result, `proto.Unmarshal` on the master gRPC server silently discarded field 13 bytes as unknown, returning `req.GetAllowedIps() == nil` and causing `saveTransportState` to fall back to the legacy minimal path (transport /30 + endpoint /32 only, no /27). Patched `file_types_proto_rawDesc` in `proto/types.pb.go` to include field 13 with correct varint length encoding — zero application code changes.

### Test Gates Added
- G14: `TestAddTunnelRequest_AllowedIpsWireRoundtrip` (`proto/proto_test.go`) — real `proto.Marshal -> proto.Unmarshal` wire gate on `AddTunnelRequest.AllowedIps`; would have caught local tracker #147 layer 4 at CI time. Added permanently to prevent descriptor/struct drift regressions.

## v1.12.8 — 2026-04-20

### Fixes
- **Master AllowedIPs admin source of truth (issue `#147` layer 3):** `MasterTunnel` gains an `AllowedIPs []string` field populated on `AddTunnel` (gRPC handler passes `req.GetAllowedIps()`) and refreshed on `UpdateTunnelPeer`. `saveTransportState` now persists `tunnel.AllowedIPs` verbatim when non-empty, falling back to local topology-aware recompute only for legacy first-boot migrations. CLI callers (`master init`, `endpoint init`, `reconcile` self-heal) now compute master-side AllowedIPs via `BuildAllowedIPsForMasterPeer` and include them in `AddTunnelRequest.AllowedIps`. Eliminates the production failure where master nodes started without `--topology` had `BuildAllowedIPsForMasterPeer(nil, …)` return minimal `[transport /30, overlay /32]` on every `saveTransportState` call, overwriting the admin-provided `/27` forwarding filter.

### Test Gates Added
- G12: `TestSaveTransportState_PersistsAdminAllowedIPs` + `TestSaveTransportState_FallbackToLocalOnEmpty` (`pkg/node/master_test.go`)
- G13: `TestAddTunnelPassesThroughAllowedIPs` (`pkg/grpc/handlers_test.go`) — pins that the gRPC handler forwards `req.AllowedIps` verbatim to `TunnelManager.AddTunnel`
- R11b: `tests/simulation/issue-92-rotation.sh` — ephemeral master without `--topology` flag, verifies `/27` present in `transport.yml` after `mesh-ctl master init`

### Additional Fixes (found during PR review)
- **`endpoint init` already-exists path used wrong AllowedIPs:** the fallback `UpdateTunnelPeer` call (taken when the master already has the tunnel) was passing endpoint-side AllowedIPs (`BuildMinimalAllowedIPsForEndpointPeer`: transport/30 + master_overlay/32) instead of master-side AllowedIPs (`BuildAllowedIPsForMasterPeer`: transport/30 + endpoint_overlay/32 + endpoints/27). This would have silently reset the master tunnel's AllowedIPs to the wrong set on any `endpoint init` re-run against an already-initialized master. Fixed to pass `masterAllowedIPs` consistently.

## v1.12.7 — 2026-04-20

### Fixes
- **Master cross-endpoint forwarding restored (issue `#147`):** master-side per-tunnel `AllowedIPs` now include the topology endpoints range (resolved via the existing `overlay.ranges[].name == "endpoints"` convention), so forwarded endpoint-to-endpoint packets pass WireGuard reverse-path validation instead of being dropped on the destination tunnel.
- **Reload and reconcile now repair existing masters:** `mesh-ctl master reload` and `mesh-ctl reconcile` actively push refreshed master-side `allowed_ips`, and same-key `UpdateTunnelPeer` calls no longer skip a requested AllowedIPs refresh.

### Test Gates Added
- G10: `TestComputeMasterPeerAllowedIPs_IncludesEndpointsRange`
- G11: R11 in `tests/simulation/issue-92-rotation.sh` verifies cross-endpoint ping matrix plus master inspect output for the endpoints-range CIDR

## v1.12.6 — 2026-04-20

### Fixes (test + docs polish)
- **Test hardening** (`pkg/node/endpoint_routes_linux_test.go`): `endpointRouteReplaceLinkWithSrc` mock now captures and asserts the actual `src` IP value, not only destination CIDRs — closes the regression gap where tests would pass with wrong source IP.
- **CHANGELOG markdown hygiene**: removed unused link reference definitions that triggered MD053.

Both findings from CodeRabbit post-merge review of PR #76 (v1.12.5).

## v1.12.5 — 2026-04-20

### Fixes
- **Root fix for endpoint↔endpoint overlay (issue `#147`):** `ConfigureTransport`
  two-loop pattern was silently overwriting the v1.12.4 `addOverlayRoutesWithSrc`
  fix — Loop 1 re-installed /27 and /25 routes without src hint immediately after
  AddPeer, Loop 2 re-installed only /32 with src. Fix: merged loops into single
  pass that always installs src hint when overlay IP is assigned.

### Changes
- `ConfigureTransport` route-install logic simplified from two-loop to single-loop
  (addresses the comment at former line 620 warning about src loss on double-install).
- `addOverlayRoutesWithSrc` helper (v1.12.4) retained at `createMasterInterface`
  eager path for first-boot visibility before the first AddPeer-driven
  `ConfigureTransport` pass.

Footnote: sim parity audit confirmed `deploy/Dockerfile.node` and
`tests/simulation/issue-92-rotation.sh` already use the production
`awg-mesh-node:local` image backed by the userspace amneziawg-go stack, so no
driver-switch change was required for v1.12.5.

## v1.12.4 — 2026-04-20

### Fixes
- **Endpoint↔endpoint overlay broken in multi-master topologies (issue `#147`):**
  per-master endpoint interfaces now install `src <endpointOverlayIP>` on
  overlay `/27` and `/25` routes, not only on `/32` host routes. This prevents
  the kernel from choosing the transport `/30` address as the packet source and
  restores cross-endpoint overlay reachability.

### Test Gates Added
- G8: endpoint↔endpoint overlay matrix + `ip route get` src assertion (R10 in
  `tests/simulation/issue-92-rotation.sh`)

## v1.12.3 — 2026-04-20

### What's New
- **Port-assignment contract fix (Pattern X):** `AddPeerResponse` now carries
  `endpoint_listen_port`; master CLI persists per-master `peer_endpoint: <host>:<port>`
  in transport.yml. Issue `#144`.
- **Self-heal migration on boot:** Master nodes now auto-heal `transport.yml` entries
  with empty `allowed_ips` on first boot after upgrade, logging `transport_state_migrated`.

### Changes
- `reconcile` now treats empty `allowed_ips` as a drift condition and forces resync
  even when pubkeys match.

### Fixes
- **Multi-master data plane dead after rolling upgrade (issue `#144`):** Second
  (and subsequent) masters silently dropped traffic because endpoint-side per-master
  interfaces bound sequential UDP ports (`:443`, `:444`, ...) while master
  `transport.yml` hardcoded `peer_endpoint: <host>:443`. The new proto field + CLI
  capture + per-master persistence fix the port contract end-to-end.
- **`allowed_ips` wiped from master `transport.yml` on image swap:** Rolling
  upgrade from v1.12.1 did not trigger `AddPeer`/`UpdateTunnelPeer`, and the
  restart path pushed empty `AllowedIPs` into runtime WireGuard state. The
  self-heal migration + `reconcile` drift detection now repair this on first
  boot and on the next admin-side reconcile.

### Test Gates Added
- G1: Persistence round-trip sim gate (R9 block in issue-92-rotation.sh)
- G3: Pubkey format-contract unit tests (8 table rows per function)
- G7: Port-assignment contract tests (unit + sim)

### Upgrade Notes
- Multi-master topologies: upgrade endpoint containers before master containers.
  After all upgrades, run `mesh-ctl master init` or `mesh-ctl reconcile` to persist
  correct per-master ports. Mixed-version clusters (pre-v1.12.3 endpoints + v1.12.3
  masters) will still experience the port-mismatch bug for the second master until
  endpoints are upgraded.

## [1.12.2] — 2026-04-19

### Fixed — endpoint side AllowedIPs dedup broke multi-master routing (local tracker #134)

Endpoints with two or more bound masters shared a single `wg0` interface and carried
every master as a peer on that interface. WireGuard's peer-selection rules require
each `AllowedIPs` entry to be unique per interface — when two master peers declared
overlapping prefixes (the transport `/30` and the mesh overlay), the kernel
deduplicated by keeping only the last peer's route. The first master's ingress
flow silently became unreachable, producing ~50 percent multi-host ping loss that
tracked cleanly to whichever master happened to be bound second. The bug was
structural, not a configuration miss — `wg-quick`, `wgctrl`, Tailscale and NetBird
all explicitly call out the same constraint.

The fix adopts the industry-standard Pattern X topology on the endpoint side:
one `wg-<master-name>` interface per bound master, each with exactly one peer and
a minimal `AllowedIPs` list of `[transport_subnet, master_overlay_ip/32]`. Endpoint↔endpoint
reachability moves from `AllowedIPs` stuffing to kernel policy routing — the endpoint
installs `ip route <other_endpoint_overlay_ip>/32 dev wg-<chosen-master>` per remote
endpoint, using the first master alphabetically that binds both sides.

This matches the master-side architecture (which already follows Pattern X with one
`wg-<endpoint-name>` per bound endpoint) — both sides are now symmetric. Migration
is transparent: on first restart the endpoint's `Run()` reconcile detects the legacy
`wg0` interface, tears it down via netlink, and rebuilds per-master interfaces from
`transport.yml`. No operator action required, no topology or CLI changes.

#### Research

Full decision record at `.agent/reports/research-endpoint-allowedips-2026-04-19.md` —
AUTHORITATIVE tier, 10 sources, weighted scoring of three candidate fixes.

#### Scope

19 files changed, 3199 insertions(+), 639 deletions(−):

| Commit | Task | Description |
|--------|------|-------------|
| `57e63b1` | T001 | `pkg/topology`: `MastersForEndpoint` helper (sorted); master-name length warning in `validate.go` |
| `bc14129` | T002 | `pkg/node`: `endpointPlatformState.ifaces` map + mutex (per-master iface registry) |
| `b8dc5b5` | T003 | `pkg/node`: `createMasterInterface` + `BuildMinimalAllowedIPsForEndpointPeer` helper |
| `27c68cb` | T004 | `pkg/node`: endpoint↔endpoint overlay routing via kernel policy routing (`endpoint_routes_linux.go` — new file) |
| `bcac520` | T005 | `pkg/node`: endpoint `Run()` reconciles N per-master interfaces on boot |
| `d9e0cd2` | T006 | `pkg/node`: transparent migration — legacy `wg0` detected, torn down, per-master interfaces rebuilt |
| `dec6904` | T007 | `pkg/topology` + `cmd/mesh-ctl/cmd/endpoint.go`: minimal `AllowedIPs` for endpoint-side master peers sent via `AddPeer` RPC |
| `a6f76d6` | T008 | `cmd/mesh-ctl/cmd/inspect.go`: new `IFACE` column (`wg-<master>`) per peer row |
| `1ab2f96` | T009+T010 | `tests/simulation/issue-92-rotation.sh`: R7 (endpoint↔endpoint via policy route, 5 assertions) + R8 (kill-master failover, 4 assertions) |
| `860ca44` | T011 | `CHANGELOG.md`: v1.12.2 entry stub (finalized by this commit) |

**Key changed files:** `pkg/node/endpoint_linux.go`, `pkg/node/endpoint_routes_linux.go` (new),
`pkg/node/endpoint_linux_test.go` (new), `pkg/node/endpoint_reconcile_test.go`,
`pkg/topology/allowedips.go`, `pkg/topology/topology.go`, `cmd/mesh-ctl/cmd/inspect.go`,
`cmd/mesh-ctl/cmd/endpoint.go`, `tests/simulation/issue-92-rotation.sh`.

#### Port range — multi-master endpoints with explicit port mapping

The per-master-iface model allocates one UDP listen port per bound master, assigned by
sorted master-name index starting at the endpoint's configured `listen_port`. An endpoint
with `listen_port: 51820` and two masters uses ports 51820 and 51821. This is only relevant
when using **explicit port mappings** (e.g. Traefik mode or bridge-network compose without
`network_mode: host`).

With `network_mode: host` (the default generated by `mesh-ctl endpoint prepare`), all host
ports are automatically accessible and no change is needed.

For Traefik or other explicit-port configurations, update the `ports:` entry to a range:

```yaml
ports:
  - "51820-51829:51820-51829/udp"   # covers up to 10 bound masters
```

This is a manual edit to the compose file — `mesh-ctl endpoint prepare` generates a single-port
entry and does not auto-compute the range (the number of bound masters is only known from the
topology, not embedded in the compose template). Single-master endpoints are not affected.

### Sim harness

Pre-fix on the reproduction topology (two masters + two endpoints): R7 **RED** — second
endpoint unreachable via policy route. Post-fix: **39/39 PASS** across R1-R8 + all
R3a-R3i subassertions.

Assertion count history:
- v1.12.0: 22 assertions (R1-R6 + R3a-R3f)
- v1.12.1: 30 assertions (+R3g-drift, R3h, R3i × 2 masters — turned harness into a real data-plane schema check)
- v1.12.2: 39 assertions (+R7 adds 5, +R8 adds 4 — endpoint↔endpoint and kill-master failover)

Migration verified by R7.3: `wg0` absent on endpoint after first boot on v1.12.2.

### Upgrade

Strongly recommended for any operator running endpoints bound to two or more masters.
Rolling upgrade from v1.12.1 is safe — the reconcile loop migrates `wg0` → per-master
interfaces on first boot (verified by sim R7.3). Single-master deployments are not
affected (they boot one interface either way).

If using explicit port mappings (Traefik / bridge network), update the endpoint compose
`ports:` entry to a range before restarting — see port range note above.

### Links

- Local tracker: #134
- Research: `.agent/reports/research-endpoint-allowedips-2026-04-19.md`
- Depends on: v1.12.1 (master-side AllowedIPs persistence) — both sides now symmetric

## [1.12.1] — 2026-04-19

### Fixed — two pre-existing critical bugs discovered during v1.12.0 multi-host beta

Both bugs predate v1.12.0 by several minor versions and were invisible to
existing unit tests and the `tests/simulation/issue-92-rotation.sh` harness
until real multi-host beta testing of v1.12.0. Upgrading from v1.12.0 to
v1.12.1 is **strongly recommended** for any operator running `mesh-ctl
master init` or `mesh-ctl upgrade --ssh`.

#### `master transport.yml` now persists `allowed_ips` per tunnel (local tracker #132)

`pkg/node/master.go::MasterRunner.saveTransportState` was writing tunnel
entries to `/config/transport.yml` without the `allowed_ips:` key. On
fresh deploys and after container restart this left amneziawg-go's peer
set with empty AllowedIPs, which silently blocks every inbound handshake
and causes 100% data-plane loss.

This bug has existed since the v1.7.0 schema-v1 introduction — it only
surfaced now because the first real multi-host beta of v1.12.0 exercised
`master init` against a fresh `/config` state. The sim harness missed it
because it only inspected the endpoint-side `transport.yml`, never the
master's.

The fix populates AllowedIPs from `computeMasterPeerAllowedIPs` — the
same `[transport_subnet, overlay_ip/32]` layout the live UAPI peer gets
from the platform-specific `buildPeerAllowedIPs` — and stamps
`SchemaVersion=CurrentSchemaVersion` so `mesh-ctl inspect` / `reconcile`
treat the state as v1.6.0-compliant rather than legacy.

#### `mesh-ctl upgrade --ssh` phaseInit now mints per-node certs (local tracker #131)

`pkg/upgrade/driver.go::phaseInit` was building `InitRequest` with only
`CaCert + Config`, omitting `NodeCert` and `NodeKey`. The Init handler
validation added in v1.6.0 (PR #15) rejects any such request with
`InvalidArgument: ca_cert, node_cert, and node_key are all required`.
As a result **every** guided upgrade since v1.6.0 failed at phase 4 on
every endpoint. Operators had to abandon `--ssh` for phase 4 and fall
back to running `mesh-ctl endpoint init <name>` by hand — which explains
why the bug went undetected until nvmd-devops became the first beta site
to actually run the guided upgrade end-to-end.

The fix mirrors the standalone `mesh-ctl endpoint init` flow: call
`pkgtls.LoadCA` + `pkgtls.IssueCert` locally on the admin workstation
and pass the resulting PEMs to Init.

#### Sim harness — new assertions that would have caught both bugs

- **R3g-drift (S-FIX-5)**: narrowed from "any DRIFT" to "disk_runtime_diverge"
  on endpoint-side inspect of master rows, which was the root cause
  signature; the broader `stale_allowed_ips` is a known architectural
  divergence (admin tracks only master's overlay /32 while runtime has
  full allowed_ips for cross-subnet routing).
- **R3h (S-FIX-1)**: reads master `/config/transport.yml` via
  `docker exec` and asserts every tunnel block contains `allowed_ips:`.
- **R3i (S-FIX-2)**: asserts `mesh-ctl inspect <master>` shows non-empty
  `DISK_IPS` and `RUNTIME_IPS` for every endpoint peer row — previously
  the sim only inspected the endpoint view.

These three assertions turn the sim from a control-plane conformance
harness into a real data-plane schema check. Running pre-fix code against
the new sim correctly produces 4 failures (R3g-drift × 2, R3h × 2);
running post-fix code produces 30/30 PASS.

#### Verification (per AGENTS.md release-gate rule)

- `go test -short -count=1 ./...` — 17 packages PASS
- `tests/simulation/issue-92-rotation.sh` on WSL2/Docker — **30/30 PASS**
  (was 22/22 with the narrower pre-v1.12.1 assertions)

### Links

- Local tracker: #131, #132
- Depends on: v1.12.0 (tier-3 rotation; not affected by either bug)
- Lineage: both bugs pre-exist v1.12.0; v1.12.0 shipped correctly but was
  "unusable for real multi-host deploy" until these fixes landed.

## [1.12.0] — 2026-04-19

### Added — Tier-3 keypair rotation (full 4-party coordinated, MINOR release)

The `mesh-ctl rotate --tier 3 <endpoint>` command now performs real keypair
rotation across the entire cluster: CLI → endpoint → every master → admin-state,
atomically. Prior to v1.12 tier-3 was documented as "full keypair rotation" but
the second `ApplyParams` call silently no-op'd (see local tracker #125) — the
new public key was never persisted at the endpoint, and masters saw a phantom
peer added next to the original.

#### How it works (4-party flow)

1. `mesh-ctl` generates a fresh Curve25519 keypair locally.
2. `mesh-ctl` calls the NEW `RotateKeypair` RPC on the endpoint: endpoint
   persists the new private key atomically (`.tmp + rename`, mode 0600) and
   rebinds amneziawg-go via UAPI `PrivateKey=<new>`. Rolls back on failure.
3. `mesh-ctl` fans out `UpdateTunnelPeer` RPCs to every master that has the
   endpoint as a peer — each master atomically does `Remove(old) + Add(new)`
   via a single UAPI transaction with restore-on-error.
4. `mesh-ctl` atomically commits the new public key to the local admin-state
   (`.tmp + rename`, no backup file).

On any master failure: CLI surfaces a structured `NAME / STATUS / DETAIL`
stderr table (STATUS ∈ {ROTATED, FAILED, REVERTED, REVERT_FAILED}) and issues
best-effort rollback. Admin-state is only committed after all masters succeed.

#### API additions

- `proto.RotateKeypair` gRPC RPC (request: `bytes private_key`, `string tunnel_name`; response: `bytes new_public_key` — derived via Curve25519 for verification)
- Endpoint-mode ONLY: master/client modes return `codes.Unimplemented`
- `NodeStatePersister` interface (`pkg/grpc/interfaces.go`) — implemented exclusively by `EndpointRunner`

#### Semantics

- **No idempotency short-circuit.** Every `mesh-ctl rotate --tier 3` call issues
  a fresh rotation, regardless of whether admin-state pubkey matches per-master
  runtime pubkey. The prior "already converged" exit path was a bug (permanent
  no-op once the cluster reached steady state) and is removed in v1.12.
- **Mutex-serialized on endpoint.** Concurrent RotateKeypair RPCs against the
  same endpoint are serialized by a per-runner mutex (NFR-5).
- **Private-key bytes never logged.** CI greps zerolog output at all levels
  (info/debug/error) for private-key leakage (NFR-1).
- **Atomic admin-state writes.** `.tmp + rename` at mode 0600; no backup file.
- **Fail-closed persistence.** `PersistKeypair` only synthesizes fresh state on
  `os.ErrNotExist`; corrupt/permission errors are propagated instead of
  silently overwriting.

#### Compatibility

- v1.12+ endpoints + masters are REQUIRED for tier-3 rotation to function
  end-to-end. v1.10+ masters already support `UpdateTunnelPeer` (local tracker #92);
  v1.11+ endpoints understand the UAPI parser fix (local tracker #128) that this
  release depends on.
- Wire protocol changes: new RPC added (backwards-compatible addition); no
  existing RPC signatures changed.
- CLI flags / topology YAML / config directory layout: no breaking changes.

#### Fixed

- Resolves local tracker #125 (tier-3 second-layer no-op — the underlying
  parser bug that caused the first v1.12.0 attempt to fail was shipped as
  v1.11.4 fix in commit 25afde0).

### Verification

- 17 Go packages green: `go test -short -count=1 ./...`
- Real-UAPI integration test: `pkg/wg/uapi_integration_test.go::TestUAPI_RotatePrivateKey_PreservesPeers` — proves amneziawg-go UAPI preserves peer table across PrivateKey swap.
- Docker e2e sim: `tests/simulation/issue-92-rotation.sh` — 12+ checks PASS on WSL2/Linux with extended R3a-R3g assertions verifying admin-state pubkey change, per-master runtime convergence, absence of old pubkey, zero drift via `mesh-ctl inspect`.
- Release gate: per AGENTS.md "RELEASE GATE — NON-NEGOTIABLE", e2e sim confirmed PASS before tag.

### Links

- Local tracker: #125
- PR: (to fill in after merge)
- Design evolution: superseded broken v1.12.0 (reverted 2026-04-19 via PR #67) — original design was architecturally correct; only the underlying parser bug prevented end-to-end success.

## [1.11.4] — 2026-04-19

### Fixed

- **`pkg/wg/uapi.go::parseDevice`** silently dropped the first peer from `Device.Peers`
  on every UAPI `get=1` response — bug existed since v0.1.0 (initial release 2026-03-27).
  The parser had a `seenDevicePublicKey` flag that intercepted the FIRST `public_key=` line
  and stored it as `Device.PublicKey`, but WireGuard UAPI never emits a device-side
  `public_key=` line. Every `public_key=` is the start of a peer entry — verified against
  `amneziawg-go/device/uapi.go::IpcGetOperation` and the WireGuard cross-platform spec
  ([wireguard.com/xplatform/](https://www.wireguard.com/xplatform/)).
  With N=1 peer (typical baseline for a fresh tunnel) the only peer was misclassified
  and lost — `dev.Peers` returned empty.

### Impact

Three callers of `iface.GetDevice()` were silently broken since v0.1.0:

- `pkg/node/master_linux.go::applyPeerKeyUpdate` → tier-3 keypair rotation:
  "existing peer for tunnel X with old public key not found in device state"
  (root cause of the v1.12.0 release that had to be reverted via PR #67).
- `pkg/node/master_linux.go::masterHandshakeChecker` → handshake timestamp lost
  for the only peer per tunnel (healthcheck false-negatives possible).
- `pkg/node/endpoint_linux.go::ListPeers` → endpoint always returned 0 peers
  via gRPC `ListPeers` RPC.

### Why no test caught it

- Old `TestParseDeviceSuccess` fed a synthetic UAPI response with a fake device-side
  `public_key=` line that real WireGuard never emits — the fake line consumed the
  `seenDevicePublicKey` trap and masked the bug.
- `mesh-ctl inspect` uses `tunnelMgr.ListTunnels()` (in-memory) for the RUNTIME column,
  NOT `GetDevice()` — so the drift report looked correct even when GetDevice() returned
  empty Peers.
- E2E sim `tests/simulation/issue-92-rotation.sh` was added in v1.10 but never run
  end-to-end before the v1.12.0 attempt — that run caught the bug for the first time.

### Changes

- `pkg/wg/uapi.go::parseDevice`: removed `seenDevicePublicKey` flag; every `public_key=`
  now opens a new peer entry. `Device.PublicKey` is derived from `Device.PrivateKey` via
  curve25519 scalar multiplication after the parse loop completes.
- `pkg/wg/uapi_test.go`: updated `TestParseDeviceSuccess` to remove the fake
  device-side `public_key=` line and assert the derived device pubkey. Added new
  `TestParseDevice_PeerCounts` with sub-tests for N=0, 1, 2, 3 peers — the N=1 case
  is the explicit regression marker.

### Verification (per AGENTS.md release-gate rule)

- `go test -short -count=1 ./...` — 17 packages PASS
- `tests/simulation/issue-92-rotation.sh` on WSL2/Docker — 12/12 PASS

### Path forward

This fix unblocks local tracker #125 (tier-3 keypair rotation). The v1.12.0 design
was correct; only the parser bug prevented end-to-end success. Re-attempt of v1.12
will land separately after this PATCH is shipped.

Local tracker #128.

## [1.10.2] — 2026-04-18

### Added

- **`mesh-ctl upgrade <version>`** — guided rolling upgrade of all cluster nodes to a
  target image version (e.g. `v1.10.2`). Five phases per node: `prepare` → `deploy` →
  `wait_ready` → `init` → `verify`. Endpoints upgraded region-by-region before masters.
  Automatic per-node rollback on `verify` failure (restores `.bak` compose, redeploys,
  reconciles). JSONL audit log at `~/.mesh-ctl/upgrade-<version>-<timestamp>.log`.
  Flags: `--dry-run` (print plan only), `--order` (override node sequence),
  `--ssh` / `--ssh-user` / `--ssh-key` (remote deploy), `--downtime-budget` (gRPC poll
  timeout), `--deploy-wait` (manual-deploy window). Local tracker issue #93.
- **`mesh-ctl upgrade status`** — reads the most recent upgrade log and prints a
  timestamped phase/status table. Local tracker issue #93.
- **`mesh-ctl upgrade compose <old-file>`** — standalone compose schema migration helper.
  Reads any older-format `docker-compose.yml`, auto-detects its schema (`v1.5.1`, `v1.6.0`,
  `v1.9.0`), and migrates it to the current schema. Without `--in-place` the result is
  written to stdout; with `--in-place` the original is saved as `<file>.bak` and the file
  is rewritten in-place. Use `--from-schema <ver>` to override auto-detection when
  heuristics are ambiguous. Migration is idempotent: current-schema files are returned
  unchanged. Local tracker issue #93.
- **`pkg/upgrade` package** — core upgrade engine:
  - `ComputeOrder` / `ComputePlan` — dependency-ordered node list (endpoints → masters,
    region-grouped, alphabetical within group)
  - `Driver` / `DriverConfig` — five-phase per-node state machine with rollback
  - `Logger` / `UpgradeLogEntry` — JSONL audit log with concurrent-safe `Append` and
    `ReadAll`; `LogPath` / `MostRecentLogPath` helpers
  - `DetectSchema` / `MigrateCompose` / `ParseSchemaVersion` — compose schema detection
    and migration (three historical schemas supported)
  - `SSHDeployer` / `DataPlaneProber` / `Reconciler` / `ComposeRenderer` function types
    injected via `DriverConfig` to decouple the engine from the CLI package

## [1.10.1] — 2026-04-18

### Added

- **`mesh-ctl inspect <node>`** — 3-column drift report comparing admin expected state,
  node disk state, and node runtime WireGuard state per peer. Connects to the node via the
  new `GetTransportState` RPC (v1.10.1+); pre-v1.10.1 nodes return a graceful
  `codes.Unimplemented` message and exit non-zero. Drift reasons surfaced:
  `key_mismatch`, `ip_mismatch`, `runtime_only`, `admin_only`. Exit code 1 when any
  drift is detected, 0 when all peers match. Local tracker issue #93.
- **`mesh-ctl reconcile`** — idempotent topology-walk that force-syncs admin expected
  state to every node. For each master calls `UpdateTunnelPeer` per bound endpoint; for
  each endpoint calls `AddPeer` per bound master (`AlreadyExists` → unchanged, not failure).
  Advisory file lock (`reconcile.lock`) prevents concurrent runs. Summary table with
  `UPDATED`, `UNCHANGED`, `FAILED`, `SKIPPED` counters per node. Exit code 1 if any node
  reports failures. Idempotent — safe to re-run after manual intervention or post-recovery.
  Local tracker issue #93.
- **`mesh-ctl status --verify-data-plane`** — opt-in L3 data-plane verification added to
  the existing `status` command. Probes each (master, endpoint) pair by calling
  `GetHealth` and `ListTunnels` concurrently per master. Structured failure reasons:
  `missing_peer`, `key_mismatch`, `handshake_timeout`, `unreachable`. Supporting flags:
  `--timeout` (per-probe, default 5 s) and `--concurrency` (max parallel master probes,
  default 4). Exit code 1 if any broken pairs are found. Local tracker issue #93.
- **`GetTransportState` gRPC RPC** on `AgentService` (`proto/agent.proto`,
  `proto/types.proto`). Returns node name, mode, overlay IP, and per-peer state
  (public key hex, allowed IPs, last handshake Unix timestamp). No private keys or PSKs
  included. Pre-v1.10.1 nodes return `codes.Unimplemented` — `mesh-ctl inspect` detects
  this and surfaces a human-readable upgrade message. Local tracker issue #93.
- **`handshakeStaleThreshold = 3 * time.Minute`** named constant in `cmd/mesh-ctl/cmd/status.go`
  for classifying WireGuard handshake age in `--verify-data-plane` probes.

## [1.10.0] — 2026-04-18

### Added

- **`UpdateTunnelPeer` gRPC RPC** on `AgentService` (`proto/agent.proto`, `proto/types.proto`).
  Masters can now update an existing tunnel's peer public key in-place without a container
  restart. Request: `name`, `peer_public_key` (32 bytes), optional `balancer_ip`, optional
  `allowed_ips`. Response: `success`, `master_public_key` (echoed), `unchanged` (idempotent
  no-op flag). Closes the propagation gap documented in `.agent/investigations/issue-92-endpoint-init-propagation.md`:
  previously, rotating an endpoint keypair left masters with the stale peer key until a
  container restart — data plane 100 % packet loss while `mesh-ctl status` reported ONLINE.
  Local tracker issue #92.
- **`MasterRunner.UpdateTunnelPeer`** implements strict C3 rollback ordering: lookup →
  same-key idempotency → capture old key → UAPI peer-replace via wgctrl → on UAPI error
  restore in-memory old key (no disk write) → on success update state → persist warn-on-failure
  (UAPI is authoritative). Kernel-level restore: if the new peer's `Configure()` fails after
  the old peer is removed, the master re-adds the old peer with its original allowed-IPs /
  endpoint / preshared key (best-effort — if restore also fails, the error surfaces with
  the full failure chain).
- **`mesh-ctl endpoint init` propagates rotated pubkeys.** When `AddTunnel` returns
  `codes.AlreadyExists` (or falls through to a string-match fallback for older servers),
  the CLI now invokes `UpdateTunnelPeer` automatically. Per-master status line printed
  for each iteration: `created`, `updated (new key: <8-hex>)`, `unchanged (key matches)`,
  or `FAILED: <error>`. Exit code non-zero if any master fails, with stderr failure
  summary and `"To recover: mesh-ctl master reload <name>"` remediation hint.
- **`mesh-ctl master reload <name>` recovery subcommand.** Walks every endpoint bound
  to the named master, reads the admin-state pubkey from `~/.mesh-ctl/nodes/<endpoint>/pubkey`,
  and force-pushes via `UpdateTunnelPeer` RPC. Idempotent; per-endpoint status line;
  non-zero exit if any RPC fails. Inherits existing mTLS + token auth — no new auth logic.
- **Pre-v1.10.0 master detection:** CLI maps `codes.Unimplemented` from older masters
  (which lack `UpdateTunnelPeer`) to a dedicated stderr message: `"master <name> running
  pre-v1.10.0 — upgrade master before rotating endpoint keys"`. Typed-code match (not
  string-based); guarded by unit test `TestUpdateTunnelPeerFailureStatus_NoStringMatch`.
- **`applyPeerKeyUpdate` platform-split** — Linux build tag uses wgctrl remove+add
  pattern matching Tier-3 rotation; `master_notlinux.go` provides the non-Linux stub.

### Changed

- `TunnelManager` interface (`pkg/grpc/interfaces.go`) gains `UpdateTunnelPeer(name,
  newPubkey [32]byte, balancerIP, allowedIPs) (unchanged bool, err error)`. Doc comment
  documents the idempotent semantics and rollback contract.

## [1.9.2] — 2026-04-18

### Fixed

- **Endpoint route-skip filter dead code** — `shouldSkipEndpointLinkRoute` had an
  unreachable self-/32 IP-equality check because the preceding `ones >= 30` guard
  swallowed all /32 prefixes. Behavior was correct for current /24-class overlay
  topologies but would silently drop legitimate /32 host routes from peers in
  future deployments. Filter now skips transport subnets `/30` and `/31` only;
  /32 self-IP check now actually reachable. Regression test
  `TestEndpointConfigureTransportInstallsNonSelfHostRoute` covers the gap.
  Discovered by post-release multi-model code review (CONSOLIDATED.md).
- **TransportConfigurator interface contract** — `allowedIPs []string` parameter
  semantics now documented as mode-dependent (endpoint installs link routes;
  client uses ECMP). Prevents future implementors from assuming uniform routing
  contract. No behavior change.
- **Endpoint reconcile log clarity** — split `peers_added` and `routes_configured`
  counters so partial failures (peer added but route install failed) are visible.
- **Endpoint package cleanup** — removed unused `endpointRouter` package-level
  singleton; function-var test seam preserved.
- Various clarifying comments around endpoint reconcile loop and route filter logic.

## [1.9.1] — 2026-04-18

### Fixed

- **Endpoint `transport.yml` peer_public_key decode** — endpoint reconcile loop previously
  called `wg.ParseKey` (base64 decoder) on a value the write path stores as hex, producing
  `"invalid key length 48: expected 32"` on every container restart — all peers silently
  dropped → data plane dead until operator re-ran `mesh-ctl endpoint init`. Decoder
  corrected to `hex.DecodeString` + `wg.NewKey` matching the write path at
  `pkg/grpc/handlers.go:436` and the client reconcile at `pkg/node/client_linux.go:508`.
  Regression guards added in `pkg/transport/state_encoding_test.go` and
  `pkg/node/endpoint_reconcile_test.go`. Local tracker issue #94.
- **Endpoint overlay routes not installed** — `ConfigureTransport` iterated
  `topology.Overlay.Ranges` (mesh-wide CIDRs) instead of per-peer `allowed_ips`, and the
  reconcile loop didn't call it at all. Three compounding bugs left endpoint→master and
  endpoint→endpoint overlay traffic with no route via `wg0`. Fix: `ConfigureTransport`
  now iterates the peer's `allowed_ips`, skips transport /30s, skips only the narrow
  self-/32 (not the containing overlay range), and installs link-scope routes via the
  new `routing.NetlinkRouter.RouteReplaceLink` helper. Reconcile calls
  `ConfigureTransport` with persisted `tt.AllowedIPs`. Route-install errors now log at
  Warn (previously Debug — silent failure hid the bug). Regression tests in
  `pkg/node/endpoint_routes_linux_test.go`. Local tracker issue #95.

## [1.9.0] — 2026-04-17

### Added

- **`--image <ref>` flag on `mesh-ctl master prepare`** — operator can pass a specific
  docker image reference (e.g. `ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.1`) that
  is written verbatim into the generated `docker-compose.yml` instead of the built-in
  `:latest` fallback.
- **`--image <ref>` flag on `mesh-ctl endpoint prepare`** — same behaviour as the master
  variant; controls the image used in the endpoint compose file.
- **`--image <ref>` flag on `mesh-ctl client prepare`** — applies to the linux client
  compose output; the MikroTik RouterOS script path is not affected (no container image
  reference in `.rsc` output).
- **`defaults.image.node` topology field** — optional string under `defaults.image` in
  `mesh-topology.yml`; sets the node image for all `master prepare` and `endpoint prepare`
  invocations that do not supply `--image`.
- **`defaults.image.client` topology field** — optional string under `defaults.image`;
  sets the client image for all `client prepare` invocations that do not supply `--image`.
- **Resolution priority** — `--image` CLI flag wins over `defaults.image.{node,client}`,
  which wins over the built-in `:latest` fallback. Existing topologies without
  `defaults.image` continue to emit `:latest` — no behaviour change for current users.

### Fixed

- Docker-built `awg-mesh-node` and `mesh-ctl` binaries now report the actual version via
  `main.versionFromBuild`, injected at build time via ldflag. Previously they reported
  `"dev"` because the Docker ldflag targeted a variable that did not exist.
  `deploy/Dockerfile.client` now injects the same ldflag as well, and
  `.github/workflows/build.yml` now derives the version string per event type: semver tag
  on tag push, `{branch}@{short-sha}` on branch push, and `pr-{N}@{short-sha}` on PR
  events instead of passing `github.sha` (40-char hex) to every build. local tracker
  issue #91.

### Notes for operators

- Pin a semver tag (e.g. `:v1.8.1`) in `defaults.image` or via `--image` for production
  deployments; reserve `:latest` for edge/dev environments where the newest build is always
  desired.
- Future workflow tweak tracked separately: add `type=ref,event=tag` to `meta-primary`/
  `meta-alias` so tag pushes also auto-publish `:v<semver>`-prefixed aliases alongside
  the existing `:<semver>` / `:<major>.<minor>` / `:<major>` / `:latest` tags. Low priority —
  current flow already produces the canonical versioned tags. See `.agent/CONTINUITY.md` on
  `main` for detail.

## [1.8.1] — 2026-04-17

CI/supply-chain hardening release. No changes to `awg-mesh-node` or `mesh-ctl`
binaries — this patch upgrades the release pipeline only. First release
published via the new `on.push.tags: ['v*']` path with full SemVer docker tag
set and SLSA provenance + SBOM attestations on every image.

### Added

- **Automatic semver docker tagging on tag push** — `.github/workflows/build.yml`
  now triggers on `v*` tags. `docker/metadata-action@v5` derives the full tag set
  (`v1.8.1`, `1.8`, `1`, `latest`, `<sha>`) for each published image. Applies to
  `awg-mesh-node`, `awg-mesh-client`, and the `awg-mesh` alias. Closes the
  architectural gap where v1.7.0 and v1.8.0 releases had no `:vN.M.P` docker
  tags at all. Via #44.
- **Retroactive retag mechanism** — `workflow_dispatch` with `retag_version` +
  `source_sha` inputs uses `docker buildx imagetools create --tag` to backfill
  semver tags onto existing manifests without rebuild. Preserves manifest digest
  bit-identically. Used to backfill v1.8.0 immediately after #44 merged. Via #44.
- **SLSA provenance + SBOM attestations** on every pushed image —
  `provenance: mode=max` (source revision, build parameters, materials) +
  `sbom: true` (SPDX). Consumers can verify via `docker buildx imagetools inspect
  --format '{{ json .Provenance }}'`. Job gained `id-token: write` +
  `attestations: write` permissions for buildx to sign attestations. Via #45.

### Security

- **All GitHub Actions pinned by commit SHA** in `build.yml` and
  `dependabot-automerge.yml` — tag-movement attacks against upstream actions
  can no longer reach this pipeline without rewriting commit history.
  Dependabot (existing `github-actions` block in `.github/dependabot.yml`)
  auto-bumps SHAs monthly back to the major-version tip via the `# vN`
  stream comment on each `uses:` line. Via #45.
- **`workflow_dispatch` input hardening** on the retag job — `retag_version`
  and `source_sha` routed through `env:` (not direct `${{ }}` interpolation)
  to prevent command injection. Whole-string `[[ =~ ]]` regex validation
  (SemVer 2.0 for version, 40-char lowercase hex for SHA) fails before any
  credential loads. Via #45.
- **`source_sha` main-reachability check** — retag job now runs `actions/checkout`
  with `fetch-depth: 0` and verifies via `git cat-file -e` + `git merge-base
  --is-ancestor "$SOURCE" origin/main` that the provided SHA is a real commit
  in this repo AND reachable from main. Closes the STRIDE Tampering path where
  a compromised `packages:write` scope could push a malicious image and retag
  it as an official release. Via #45.

### Fixed

- **`Verify multi-arch manifest` step** now uses the bare commit SHA tag
  (matches existing registry naming convention) instead of the prior
  `sha-<sha>` prefix attempt. Via #44.

### Notes for operators

- Pulling `ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.1` (or `:1.8`, `:1`)
  now works end-to-end — no retag dispatch needed for this or future releases.
- The `retag` workflow remains available as backfill safety for any releases
  published before v1.8.1 that lack semver docker tags.
- Image pulls can be cryptographically verified against their SLSA provenance.

### Merged PRs

- #44: ci: semver docker tagging + retroactive retag
- #45: ci: harden workflow — SHA pinning, input validation, provenance/SBOM, source_sha verification

## [1.8.0] - 2026-04-17

Internal review hardening — closes 5 open issues (#20, #21, #23, #24, #25) from the
pre-v1.7.0 code review with zero new runtime dependencies.

### Fixed

- **ICMP healthcheck demux rewrite** — shared raw ICMP socket per `HealthChecker` with
  demux-by-seq routing, eliminating cross-goroutine packet starvation on Linux and
  bounding the per-call read loop. Closes [#20] (C2 CRITICAL) and [#25] (M5 MEDIUM) via
  #40. See `docs/adr/0006-icmp-shared-socket-demux.md`. Race-free `socketMu sync.RWMutex`
  + `sync.Once` Close idempotency + atomic `seqCounter` on the hot path.
- **Plaintext token no longer on stdout by default** — `--show-token` flag gates the
  emit; default path writes a confirmation to stderr pointing at the on-disk token
  (mode 0600). WARN log fires when `--show-token` is set to warn about
  shell-history/tmux leak. Closes [#21] (H2 HIGH) via #39.
- **DSCP range validation** (1..63) — `topology.ValidateDSCP` rejects out-of-range
  values at topology load AND codegen, preventing `tableID = 100 + DSCP` from clobbering
  Linux kernel-reserved tables 253 (default) and 254 (main). Closes [#23] (M2 MEDIUM)
  via #37.
- **Typed YAML corrupt-state sentinel** — `ErrCorruptNodeState` replaces fragile
  `strings.Contains` classification in `EnsureKeypair`. Extended pattern to
  `ErrCorruptTransportState` and `ErrCorruptClientState`. Closes [#24] (M4 MEDIUM)
  via #38. See `docs/adr/0007-typed-error-sentinel-for-yaml.md`.
- **`mesh-ctl bootstrap` command injection prevention** (discovered in PR #41 review) —
  `validateImageRef` rejects shell metacharacters in `--image`; `shellQuote` single-quotes
  the value at all remote execution sites.
- **`SaveTokenHash` directory permissions** tightened from 0755 to 0700 (discovered in
  PR #39 review).

### Added

- **`mesh-ctl bootstrap --host IP`** — SSH-based VPS provisioning: installs Docker (if
  missing) and pulls the `awg-mesh-node` image. Strict host-key verification via
  `~/.ssh/known_hosts` by default; `--accept-new-host-key` flag for first contact. SSH
  agent preferred over on-disk key. T006a from the original awg-mesh spec, via #41.
- **`--show-token` flag** on `mesh-ctl token rotate`, `master prepare`, `endpoint
  prepare`, `client prepare`. Default false — token goes only to disk.
- **`topology.ValidateDSCP(dscp int) error`** + `ErrInvalidDSCP` sentinel — library
  surface for DSCP range validation.
- **`pkg/node.ErrCorruptNodeState`**, **`pkg/transport.ErrCorruptTransportState`**,
  **`pkg/node.ErrCorruptClientState`** — typed sentinels for `errors.Is` classification.
- **`HealthChecker.Start()`** / **`Close()`** lifecycle methods + shared socket +
  demuxLoop goroutine.
- **`Makefile` target `grep-token-leak`** + CI step in `build.yml` — guards against
  future regressions that reintroduce unguarded `fmt.Printf` on tokens.
- **`docs/adr/0006-icmp-shared-socket-demux.md`** and **`docs/adr/0007-typed-error-sentinel-for-yaml.md`**
  with full context, decision drivers, rollback paths, and alternatives-considered sections.
- **`docs/MIGRATION.md`** — operator guide for migrating from the legacy 5× MikroTik
  layout to `awg-mesh` 2× master + endpoints + clients. T079 from the original awg-mesh
  spec.
- **`tests/v18_smoke/`** — smoke + e2e Docker fixture (`smoke.sh`, `e2e.sh`, `compose.yml`,
  `build.sh`, `README.md`). Release gate: `make release-gate` runs both before tag
  creation, per operator directive.
- 20+ new tests covering every FR and regression path — `TestValidateDSCP`,
  `TestEnsureKeypairRecoversTruncatedYAML`, `TestLoadNodeStateCorruptSentinel`,
  `TestTokenRotate_*` (3 tests), `TestPingICMPConcurrentDemux`,
  `TestPingICMPBoundedReadLoop`, `TestValidateImageRef`, `TestBootstrapHelpFlags`, etc.

### Breaking Changes

- **`mesh-ctl token rotate`, `master prepare`, `endpoint prepare`, `client prepare` no
  longer print the raw bearer token to stdout by default.** Operators who relied on
  piping the command output into downstream tools MUST either (a) read the token from
  its persisted path (`cat <config-dir>/nodes/<name>/token`, mode 0600), or (b) pass
  `--show-token` to restore the old stdout behavior (NOT recommended — token becomes
  visible in shell history, `ps` arg list, tmux scrollback).
- **Topology files with `routing_policies[].dscp` outside 1..63 are now rejected at
  load time.** Previous silent behavior could clobber kernel tables 253/254.

### Migration from v1.7.0

No mandatory action. Drop-in upgrade:

```
docker pull ghcr.io/coonfuuseed-paandaa/awg-mesh-node:v1.8.0
docker pull ghcr.io/coonfuuseed-paandaa/awg-mesh-client:v1.8.0
go install github.com/coonfuuseed-paandaa/awg-mesh/cmd/mesh-ctl@v1.8.0
```

See `docs/MIGRATION.md` for the legacy MikroTik → awg-mesh migration path if you are
still on the pre-v1.0 layout.

If your operator scripts parse stdout of `mesh-ctl ... prepare` or `token rotate` for
the token, update the parser to read `<config-dir>/nodes/<name>/token` instead, or add
`--show-token` to the invocation.

### Merged PRs

- [#37] fix: DSCP range validation 1-63 (closes #23) — `572493f`
- [#38] refactor: typed YAML error sentinel (closes #24) — `bbdea1b`
- [#39] fix: gate plaintext token behind --show-token flag (closes #21) — `7965b14`
- [#40] fix: shared-socket ICMP demux + bounded read loop (closes #20, #25) — `44f8797`
- [#41] feat(mesh-ctl): add bootstrap --host command for VPS provisioning — `1a91f95`
- [#42] test(v18): smoke + e2e docker fixture for v1.8.0 release gate
- [#43] chore: release v1.8.0 (ADRs, CHANGELOG, README, MIGRATION)

[#20]: https://github.com/coonfuuseed-paandaa/awg-mesh/issues/20
[#21]: https://github.com/coonfuuseed-paandaa/awg-mesh/issues/21
[#23]: https://github.com/coonfuuseed-paandaa/awg-mesh/issues/23
[#24]: https://github.com/coonfuuseed-paandaa/awg-mesh/issues/24
[#25]: https://github.com/coonfuuseed-paandaa/awg-mesh/issues/25
[#37]: https://github.com/coonfuuseed-paandaa/awg-mesh/pull/37
[#38]: https://github.com/coonfuuseed-paandaa/awg-mesh/pull/38
[#39]: https://github.com/coonfuuseed-paandaa/awg-mesh/pull/39
[#40]: https://github.com/coonfuuseed-paandaa/awg-mesh/pull/40
[#41]: https://github.com/coonfuuseed-paandaa/awg-mesh/pull/41
[#42]: https://github.com/coonfuuseed-paandaa/awg-mesh/pull/42

## [1.7.0] - 2026-04-17

### Added
- **Client-side ECMP hardening** (`.agent/specs/client-ecmp/`):
  - `TunnelTransport` now persists `AllowedIPs`, `PersistentKeepalive`, and `SchemaVersion` — the peer's AllowedIPs from `AddPeer` are restored verbatim on reconcile instead of being hardcoded to `0.0.0.0/0` (#27)
  - Unified client ECMP code path: `rebuildClientECMP` applies health filter + sticky sessions + L4 hash unconditionally; no more divergent legacy vs VIP semantics (#30)
  - `EnableStickyECMP` is now CIDR-scoped; `DisableStickyECMP` actually removes per-CIDR rules; runtime `balancer_ip` changes produce clean state (#32)
  - Deterministic client interface names `wg-c<hash>` from peer pubkey SHA-256; stable across restarts. Legacy `wg-cN` interfaces cleaned up on reconcile (#31)
  - Partial-mesh boot tolerance: reconcile errors no longer fatal to `Run()`; structured logs at every ECMP/sticky decision (#33)
  - Docker-compose fixture + verify.sh for manual US1/US2 regression tests under `tests/client_ecmp/` (#28)

### Migration from v1.6.0

- First boot with a pre-v1.6.0 `transport.yml` logs one WARN and applies fallback defaults (`allowed_ips=0.0.0.0/0`, `keepalive=25s`); migration is durable — the WARN does NOT fire on subsequent boots.
- Operators with running clients do NOT need to re-run `mesh-ctl client init`; the client updates its own state file on next reconcile.
- Client interface names change from `wg-c0`/`wg-c1` to pubkey-derived `wg-c<4hex>`. External monitoring that scrapes interface names must be updated.
- H4 finding (hardcoded `allowedIPs=["0.0.0.0/0"]`) is now closed (filed as #22).

## [1.6.0] - 2026-04-17

### Fixed
- 48-finding audit remediation across security, correctness, and quality (batches 1–4)
- Production deployment field report (2026-04-17) — `mesh-ctl <role> prepare` now
  generates a compose file that actually starts on a clean Ubuntu host:
  - Host-network templates no longer set container sysctls (runc rejected them)
  - All templates mount `/dev/net/tun` — required for TUN device creation
  - Templates embed `MESH_TOKEN_HASH` (bcrypt) instead of plaintext `MESH_TOKEN`;
    the node bootstraps `/config/mesh.token` from that env var on first boot
  - Host volume now binds to `/config` (the binary's default ConfigDir), not
    `/var/lib/awg-mesh:/var/lib/awg-mesh`
  - `MESH_NAME` and `MESH_CONFIG_DIR` now present in every template
- `mesh-ctl master init` no longer emits alarming "warning:" noise when an
  endpoint has not yet been prepared; partial-rollout skips are reported once
  on stderr as "note: endpoint %q not yet prepared"

### Added
- Node binary reads `MESH_*` env vars as fallbacks for every CLI flag (12-factor
  container config). Flags still win when explicitly set. Env names:
  `MESH_MODE`, `MESH_NAME`, `MESH_OVERLAY_IP`, `MESH_LISTEN_PORT`,
  `MESH_CONFIG_DIR`, `MESH_TOPOLOGY`, `MESH_LOG_LEVEL`, `MESH_METRICS_ADDR`.
- First-boot bootstrap: the node writes `<config>/mesh.token` from
  `MESH_TOKEN_HASH` when the file is absent. Invalid bcrypt input fails fast
  so plaintext tokens cannot silently lock the node out.
- Template contract tests (`cmd/mesh-ctl/cmd/templates_test.go`) — pin the
  structural invariants (no sysctls on host net, `/dev/net/tun` mounted,
  `MESH_TOKEN_HASH` embedded, `MESH_NAME` present, `/config` volume) so this
  class of deployment regression is caught in CI.

### Refactored
- Extract transport state types to `pkg/transport` (T016)

### CI
- Privileged tests, govulncheck, and coverage merge added to CI pipeline
- Multi-arch Docker builds via buildx — `linux/amd64`, `linux/386`,
  `linux/arm64`, `linux/arm/v7`, `linux/arm/v6`. Covers the realistic
  hardware set: Intel/AMD servers, legacy 32-bit x86, MikroTik (arm64),
  Raspberry Pi 3B+/4/5 (arm64), Raspberry Pi 2/3 32-bit (arm/v7), and
  Raspberry Pi Zero/1 (arm/v6). Closes the ADR-0001 gap that non-amd64
  hardware could not pull `awg-mesh-client:latest`. CI verifies the
  pushed manifest list advertises every platform.

### Migration from v1.5.0

Operators running compose files produced by `mesh-ctl prepare` under v1.5.0 must
re-run `prepare` before upgrading the node image. The old templates embedded a
plaintext `MESH_TOKEN` env var that this release's binary no longer reads — the
new binary expects `MESH_TOKEN_HASH` (bcrypt) and bootstraps `/config/mesh.token`
from it on first boot. Without the regenerated compose, `mesh-ctl <role> init`
will fail authentication because no token hash is present on the node.

Steps per already-deployed node:

1. `mesh-ctl <role> prepare <name>` on the admin workstation (generates new compose
   with `MESH_TOKEN_HASH`).
2. On the target host: stop the old container, replace the compose file, delete
   the old `/config/mesh.token` (if any), then `docker compose up -d` with the
   new image.
3. `mesh-ctl <role> init <name>` — the new bearer token printed by `prepare`
   is used once, and the bootstrap writes it into `/config/mesh.token`.

---

## [1.5.0] - 2026-04-07

### Added
- Client state persistence wired into `ClientRunner` lifecycle
  - `SaveClientState` / `loadClientState` for DSCP routing and DNS configuration
  - Client survives restart without topology file present
  - DNS server startup from persisted state on client init

---

## [1.4.0] - 2026-04-05

### Added
- `--traefik` flag for `docker-compose` generation — TCP services exposed via Traefik labels, UDP traffic routed directly
- ADR-0003: Traefik integration hybrid pattern (TCP via Traefik, UDP direct)

### Fixed
- Connmark DSCP fix — save/restore `fwmark` via conntrack for return traffic (was dropped on asymmetric paths)

### Docs
- Traefik integration guide with hybrid TCP/UDP pattern
- Direct Overlay Routing section with MikroTik examples
- RouterOS 7.21 minimum requirement documented; DSCP routing corrections

---

## [1.3.0] - 2026-04-02

### Added
- Two Docker images: `awg-mesh-client` (CGO-free, ~15 MB) and `awg-mesh-node` (full, ~42 MB)
- WAN interface auto-discovery via netlink default route (MikroTik ROS 7.20+ compatible)
- `MESH_INTERFACE` environment variable for manual interface override
- Client state persistence to `/config/client-state.yml` for zero-config restart
- `nocapture` build tag for CGO_ENABLED=0 client binary builds
- CI matrix: parallel builds for client and node images with separate smoke tests
- ADR-0001: Multi-Image Docker Strategy
- ADR-0002: MikroTik VETH Interface Discovery

### Changed
- Dockerfile renamed to Dockerfile.node
- Docker CI builds both images in matrix
- `awg-mesh:latest` is now an alias for `awg-mesh-node:latest`

### Fixed
- Hardcoded `eth0` in master exit mode replaced with auto-discovered interface
- golangci-lint updated to v2.11.4 (Go 1.25 compatible)
- Root skip guards added to privileged tests for CI compatibility
- All errcheck findings resolved for golangci-lint v2

---

## [1.2.0] - 2026-03-31

### Added
- Smart Client — DSCP-based policy routing: single container replaces N per-region AWG containers
- Embedded DNS server (miekg/dns) — A/PTR records for overlay zone, upstream forwarding, hot-update
- `mesh-ctl routing generate` command — generates platform-specific router configurations:
  - MikroTik `.rsc` scripts with mangle rules, routing table creation, VPN routes
  - Linux shell scripts with iptables DSCP marking, ip rule, ip route (numeric table IDs)
  - Generic JSON with DSCP map and fallback overlay-IP static routes
- Master exit mode — `exit: true` in topology enables masquerade for direct VPN egress
- Topology extension: `routing_policies`, `dns`, `exit` fields in mesh-topology.yml
- DSCP teardown on client shutdown — nftables rules and ip rules cleaned up gracefully
- Fallback overlay-IP routes in generic JSON for routers without DSCP support

### Changed
- DNS server reuses forwarding client (prevents per-request socket allocation)
- MikroTik .rsc scripts now include `/routing/table add` for RouterOS v7 compatibility

### Fixed
- DSCP per-table routes now populated from transport state at startup
- Linux script uses numeric table IDs (100+DSCP) matching kernel-side implementation
- Zone builder skips IPv6 addresses (A/PTR records support IPv4 only)
- Empty DNS zone rejected at creation time (fail-fast instead of silent normalization)

---

## [1.1.0] — 2026-03-30

### Added
- **Idempotent endpoint init** — `AddPeer` proceeds even when `AddTunnel` returns "already exists", resolving nil pointer on re-initialization
- **Overlay route propagation** — endpoints install routes for client and master overlay traffic through WG tunnels
- **E2E simulation test suite** — automated 8-node verification: `WGHandshake`, `OverlayPing`, `ECMP`, `ClientToMaster`, `Status`
- `NetlinkRouter` Linux-only tests covering loopback, address, and route operations
- `NftablesFirewall` Linux-only tests covering NAT, MSS clamping, and connmark

### Changed
- `Client.rebuildECMP` fully migrated to netlink + nftables (no remaining exec.Command calls)

### Fixed
- Nil pointer panic when re-initializing an endpoint after `AddTunnel` returns "already exists"
- Master public key fallback — reads from disk when `AddTunnel` response is unavailable during endpoint init

### Removed
- **443 LOC of exec.Command-based routing code** — all fork-based routing stubs eliminated from the routing package

---

## [1.0.0] — 2026-03-30

Major architectural milestone: the routing layer is fully refactored from `exec.Command` subprocess invocations to kernel-native APIs with zero fork overhead.

### Added
- **`vishvananda/netlink`** — all route, address, and link operations via netlink socket
- **`google/nftables`** — NAT masquerade, TCP MSS clamping, connmark sticky sessions via nftables Go API
- **`cilium/ebpf`** — TC program loader for inter-WG-interface forwarding on master nodes
- **`Router`, `Firewall`, `Sysctl` interfaces** — mockable, testable abstractions for all kernel operations
- **E2E simulation test suite** — 5 subtests covering WG handshake, overlay ping, ECMP, client-to-master connectivity, and node status
- New packages: `pkg/routing/netlink.go`, `pkg/routing/nftables.go`, `pkg/routing/sysctl.go`, `pkg/ebpf/forwarder.go`, `pkg/ebpf/bpf/forward.c`

### Changed
- All routing operations (ECMP multipath, address assignment, interface bring-up) use netlink syscalls instead of `ip route` / `ip addr` subprocesses
- NAT masquerade and TCP MSS rules use nftables API instead of `iptables` subprocess
- eBPF TC program replaces `ip rule` / `ip route` for inter-WG-interface forwarding on masters

### Removed
- All `exec.Command` invocations from the routing and firewall layer

---

## [0.9.0] — 2026-03-29

All 15 investigation findings resolved (4×P0, 6×P1, 5×P2). Zero known defects.

### Fixed
- **CaptureScheduler goroutine leak** — scheduler stopped cleanly via `doneCh` handshake on node exit
- **MTU from topology** — reads `physical_mtu` / `awg_overhead` from config instead of hardcoded 1420/80
- **TLS cert caching** — mtime-based cache for both `node.crt` and `node.key`; reloads on file change
- **Client init on zero masters** — returns error instead of silently succeeding when no masters are available
- **Healthcheck WG handshake fallback** — if ICMP fails but the last WG handshake is recent, the tunnel is considered alive

---

## [0.8.0] — 2026-03-29

### Added
- **Transport pool `Deallocate`** — `/30` subnets are returned to the pool when an endpoint is removed
- `TestRotateParamsAppliesNewPublicKey` — verifies tier 3 rekey applies the new public key
- `TestDeallocate` — verifies transport pool reclamation

### Fixed
- **Token rotation** — auth interceptor reloads token hash from disk via mtime-cached provider; previously the hash was captured once at startup and never refreshed
- **Master restart routing preservation** — `transport.yml` now persists `overlayIP` and `balancerIP` per tunnel; reconciliation fully restores overlay routes and ECMP on restart
- **Tier 3 rekey** — `RotateParams` handler applies `NewPublicKey` via UAPI (was silently dropped)
- **Atomic token write** — `RotateToken` RPC and `SaveTokenHash` write via temp file + atomic rename
- **`RemoveTunnel` cleanup** — clears overlay route and rebuilds ECMP before closing the interface
- **`AddTunnel` ECMP race** — sets `Healthy=false` until interface creation succeeds, preventing premature ECMP inclusion
- **Import cycle** — `pkg/grpc` ↔ `pkg/node` type alias reverted to synchronized structs

---

## [0.7.0] — 2026-03-28

### Added
- **Native ICMP healthcheck** — replaced `exec.Command("ping")` with `golang.org/x/net/icmp`; all targets pinged in parallel (10 targets complete in ~502 ms vs 5 s sequential)
- **Stale failure purge** — healthcheck failure counters cleared on tunnel removal, preventing false "down" state on recreated tunnels
- `golang.org/x/net` and `golang.org/x/sync` promoted to direct dependencies

### Fixed
- **`AddPeer` race condition** — mutex held across `configurePeerOnIface` for existing peers; `byKey` map initialized in constructor
- **UAPI goroutine leak** — 30-second connection deadline on UAPI socket prevents indefinite goroutine hang
- **Integration test** — added `mesh.token` seed and privileged mode for Docker Desktop compatibility

### Changed
- `PingOverlay` delegates to `PingICMP` (backward compatible)
- `purgeStaleFailures` runs on every healthcheck tick

---

## [0.6.0] — 2026-03-28

### Added
- **Client-side ECMP** — multipath route to masters' `balancer_ip/32` with health-aware nexthop management
- **Conntrack sticky sessions** — `iptables -t mangle` connmark rules (save on NEW, restore on ESTABLISHED) keep TCP connections pinned to the same master across ECMP rebalancing
- **L4 ECMP hash** — `fib_multipath_hash_policy=1` sysctl distributes flows by src:port + dst:port
- **Client healthcheck** — pings master transport IPs; removes unhealthy nexthops and restores them on recovery
- **`HealthTarget` interface** — generalized healthcheck abstraction replaces hard-coded `MasterTunnel`, works for both master and client modes
- `balancer_ip` field added to `AddPeerRequest` proto; all transport fields in generated `.pb.go`
- `BalancerIPForAddr()` in `topology/ranges.go` for range-based balancer IP lookup
- `DisableStickyECMP` cleans up mangle rules when all nexthops are removed

### Fixed
- **Idempotent connmark rules** — `iptables -C` check before `-A` prevents rule duplication on repeated `rebuildECMP` calls
- **Healthcheck callback race** — `balancerIP` captured under mutex in healthcheck callbacks
- Silent error drops replaced with proper logging in `mesh-ctl client init`

---

## [0.5.0] — 2026-03-28

### Added
- **Full mesh data plane verified** — WG handshakes validated across 7-node Docker simulation (2 masters × 5 endpoints, 10 tunnels, overlay ping 0% loss)
- **Peer key exchange** — `Init` returns `NodePublicKey`; `AddTunnel` returns `MasterPublicKey` for bidirectional WG peering
- **TCP MSS clamping** — `--clamp-mss-to-pmtu` on endpoints prevents fragmentation through WG tunnels
- **`peer_host` topology field** — separates gRPC management address from WG peering address; enables Docker simulation with internal IPs
- **Endpoint `ConfigureTransport`** — assigns transport `/30` IPs to `wg0` after `AddPeer`
- `KeyProvider` interface injected into `AgentHandler` for public key retrieval
- `PeerAddr()` method on `MasterNode` / `EndpointNode` (falls back to `Host` when `peer_host` not set)
- `ClampMSSToPMTU()` in `routing/mss.go`

### Fixed
- **WG interfaces UP before routing** — `setInterfaceUp()` called before address/route operations; fixes "Nexthop has invalid gateway"
- **gRPC TLS bootstrap** — `GetConfigForClient` now includes `GetCertificate`; fixes "unrecognized name" TLS error
- **Empty public key in peer exchange** — `Init` and `AddTunnel` now return keys
- **Endpoint transport IPs missing** — `ConfigureTransport` adds `/30` per master to `wg0`
- `client_other.go` `RemovePeer` returns error consistently with `AddPeer`
- Master sets WG peer endpoint address in `createTunnelInterface`

---

## [0.4.0] — 2026-03-28

### Added
- **Transport/overlay separation** — WG tunnels use auto-allocated `/30` subnets; overlay IPs on loopback (correct WG point-to-point model)
- **Transport address allocator** (`pkg/transport/`) — `/30` per tunnel from a configurable pool
- **Bidirectional peer exchange** — `mesh-ctl init` configures both sides of each tunnel
- **ECMP routing** — balancer IPs route to healthy endpoints via weighted nexthops
- **Healthcheck → routing integration** — `onDown` removes routes and rebuilds ECMP; `onUp` restores them
- **Endpoint NAT** — `EnableMasquerade` + `EnableForwarding` wired on startup
- **State reconciliation** — nodes reconstruct interfaces from saved state on restart
- **Client transport** — Linux clients connect to masters via transport `/30`s with ECMP
- **Autonomous capture scheduler** — masters run packet capture on schedule without requiring the admin PC
- **`mesh-ctl config show`** — displays transport pool and current allocations
- **`mesh-ctl status`** — shows transport IPs per tunnel
- Domains distributed from admin PC to masters via gRPC `CaptureRequest`

### Changed
- Topology YAML requires a `transport:` section (`pool` + `prefix_length`)
- `AddTunnel` / `AddPeer` proto messages include new transport fields
- Overlay IPs are no longer on WG interfaces (transport IPs used instead)
- Client no longer creates interfaces from topology on startup (uses gRPC + reconciliation)

### Fixed
- Cross-platform paths — `filepath.Join` throughout the codebase
- Version detection without ldflags — uses `debug.ReadBuildInfo()`
- Docker image name mismatch — `mesh-ctl` uses `ghcr.io/coonfuuseed-paandaa/awg-mesh` (matches CI)

---

## [0.3.0] — 2026-03-28

### Added
- **Full 7-node simulation** — configurable `grpc_port` enables multi-node Docker Compose simulation on a single host
- **gRPC insecure mode** — pre-`Init` bootstrap without mTLS for initial onboarding flow

### Changed
- `mesh-ctl install` installs only `mesh-ctl` (admin PC tool); `make install-all` for both binaries
- Platform-specific install docs: `make install` for Linux/macOS, `go install` for Windows

### Fixed
- Cross-platform paths — `filepath.Join` in `loadNodeHost()` (fixes Windows mixed slashes)
- `net.ParseCIDR` return values — corrected for Linux compile compatibility

---

## [0.2.0] — 2026-03-28

### Added
- **Autonomous capture scheduler** — masters run packet capture on schedule without the admin PC
- **`mesh-ctl config show`** — inspect config directory, CA status, and node states
- **Version resolution** — `mesh-ctl version` shows real version via `debug.ReadBuildInfo()` (ldflags → module version → `dev`)
- `CaptureRequest` proto: `schedule` and `retention_days` fields
- README operational deployment guide (Getting Started, Updating, Docker integration)
- README.ru.md — full Russian translation
- MIT LICENSE
- Dependabot: weekly Go deps, monthly GitHub Actions updates
- CI: coverage threshold gate (40%) and Docker smoke test on every PR

### Fixed
- Cross-platform paths — `filepath.Join` throughout (fixes mixed slashes on Windows)
- Docker image name — `mesh-ctl` now uses `ghcr.io/coonfuuseed-paandaa/awg-mesh` (matches CI)
- `--topology` default documented as empty string (not `/config/mesh-topology.yml`)
- `domains_file` documented as a local path on the admin PC, not a container path

---

## [0.1.0] — 2026-03-27

Initial release of awg-mesh — a Docker-native encrypted overlay mesh network built on AmneziaWG.

### Added
- **Unified node binary** (`awg-mesh-node`) running in three modes: `master`, `endpoint`, `client`
- **`mesh-ctl`** CLI for topology management, rotation, capture, and onboarding
- **Topology-as-code** — single `mesh-topology.yml` as the source of truth
- **AmneziaWG overlay mesh** — encrypted tunnels with DPI obfuscation via `amneziawg-go` library
- **gRPC management plane** — 14 RPCs with mTLS + bearer token dual auth, dynamic cert hot-reload
- **Three-tier AWG rotation** — automated anti-DPI parameter rotation (Tier 1: junk, Tier 2: S/H headers, Tier 3: full keypair)
- **Protocol family mimicry** — gopacket TLS/QUIC capture for realistic traffic fingerprinting
- **Two-level ECMP** — load balancing across masters and across endpoints
- **Health-checked failover** — ping-based healthcheck with auto-remove / re-add
- **MikroTik RouterOS support** — `.rsc` script generation for containerized clients
- **IP range management** — CLI commands for overlay address space operations
- **Prometheus metrics** — `:9091/metrics` with tunnel, peer, rotation, and healthcheck gauges
- **Structured JSON logging** — zerolog with component scoping and configurable levels
- **Docker-native** — 42 MB Alpine image at `ghcr.io/coonfuuseed-paandaa/awg-mesh`
- CI: GitHub Actions pipeline (lint → test → build → Docker smoke test)
- E2E verified: two-container AWG tunnel, 3/3 ping, 0% loss

---

[Unreleased]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.12.7...HEAD
[1.12.2]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.12.1...v1.12.2
[1.12.1]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.12.0...v1.12.1
[1.12.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.11.4...v1.12.0
[1.11.4]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.10.2...v1.11.4
[1.10.2]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.10.1...v1.10.2
[1.10.1]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.10.0...v1.10.1
[1.10.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.9.2...v1.10.0
[1.9.2]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.9.1...v1.9.2
[1.9.1]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.9.0...v1.9.1
[1.9.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.8.1...v1.9.0
[1.8.1]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.8.0...v1.8.1
[1.8.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.7.0...v1.8.0
[1.7.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.6.0...v1.7.0
[1.6.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.5.0...v1.6.0
[1.5.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.4.0...v1.5.0
[1.4.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.3.0...v1.4.0
[1.3.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.2.0...v1.3.0
[1.2.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.1.0...v1.2.0
[1.1.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.9.0...v1.0.0
[0.9.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/coonfuuseed-paandaa/awg-mesh/releases/tag/v0.1.0
