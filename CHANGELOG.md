# Changelog

All notable changes to awg-mesh are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

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
- Docker image name mismatch — `mesh-ctl` uses `ghcr.io/thebtf/awg-mesh` (matches CI)

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
- Docker image name — `mesh-ctl` now uses `ghcr.io/thebtf/awg-mesh` (matches CI)
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
- **Docker-native** — 42 MB Alpine image at `ghcr.io/thebtf/awg-mesh`
- CI: GitHub Actions pipeline (lint → test → build → Docker smoke test)
- E2E verified: two-container AWG tunnel, 3/3 ping, 0% loss

---

[Unreleased]: https://github.com/thebtf/awg-mesh/compare/v1.1.0...HEAD
[1.1.0]: https://github.com/thebtf/awg-mesh/compare/v1.0.0...v1.1.0
[1.0.0]: https://github.com/thebtf/awg-mesh/compare/v0.9.0...v1.0.0
[0.9.0]: https://github.com/thebtf/awg-mesh/compare/v0.8.0...v0.9.0
[0.8.0]: https://github.com/thebtf/awg-mesh/compare/v0.7.0...v0.8.0
[0.7.0]: https://github.com/thebtf/awg-mesh/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/thebtf/awg-mesh/compare/v0.5.0...v0.6.0
[0.5.0]: https://github.com/thebtf/awg-mesh/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/thebtf/awg-mesh/compare/v0.3.0...v0.4.0
[0.3.0]: https://github.com/thebtf/awg-mesh/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/thebtf/awg-mesh/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/thebtf/awg-mesh/releases/tag/v0.1.0
