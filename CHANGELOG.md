# Changelog

All notable changes to awg-mesh are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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

[Unreleased]: https://github.com/coonfuuseed-paandaa/awg-mesh/compare/v1.10.2...HEAD
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
